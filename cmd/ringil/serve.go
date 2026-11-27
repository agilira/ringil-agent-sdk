// serve.go: the agent behind a local HTTP API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"
	"golang.org/x/term"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/a2a"
	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/eventlog"
	"github.com/agilira/ringil-agent-sdk/metrics"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/serve"
)

// tokenFile holds the API's token, private to the user.
const tokenFile = "serve-token"

// cmdServe answers the HTTP API on a loopback address until Ctrl-C. Each
// run gets an agent of its own, built like the one of `ringil run`; the
// MCP servers and remote agents are connected once and shared. Calls the
// chat would confirm wait for a decision through the API.
func (c *cli) cmdServe(ctx *orpheus.Context) error {
	defer c.shutdown()
	l, err := c.serveListener()
	if err != nil {
		return err
	}
	c.serveBoard()
	srv, where, err := c.newServer(ctx)
	if err != nil {
		closeQuietly(l)
		return err
	}
	c.server = srv
	defer srv.Close()
	if c.conf().Serve.A2A {
		if err := c.exposeA2A(srv, a2aProfile(ctx), l.Addr().String()); err != nil {
			return err
		}
	}
	run, stop := signal.NotifyContext(ctx.Context(), os.Interrupt)
	defer stop()
	c.imports.stop = stop // an import from the app stops serve, which then imports and starts again
	stopAll, err := c.startBackground(ctx, run, srv)
	if err != nil {
		return err
	}
	defer stopAll()
	c.note("serving on http://" + l.Addr().String() + "/v1/ ; bearer token " + where)
	if c.inContainer() {
		c.note("  * in a container: listening on every interface of its network, answering the loopback and the host (" + c.peersText() + ") only")
	}
	c.appLink(ctx, srv, l.Addr().String())
	return srv.Serve(run, l)
}

// startBackground starts what works beside the API: the triggers (unless
// serve.no_triggers) and the chat apps. The function returned stops
// them.
func (c *cli) startBackground(ctx *orpheus.Context, run context.Context, srv *serve.Server) (func(), error) {
	stopChannels, err := c.startChannels(ctx, run, srv)
	if err != nil {
		return nil, err
	}
	stopTriggers := func() {}
	if !c.conf().Serve.NoTriggers {
		if stopTriggers, err = c.startTriggers(ctx, run, srv); err != nil {
			stopChannels()
			return nil, err
		}
	}
	return func() { stopTriggers(); stopChannels() }, nil
}

// newServer builds the server: its token, the shared remote tools, the
// session store and the profiles. It returns where the token is.
func (c *cli) newServer(ctx *orpheus.Context) (*serve.Server, string, error) {
	token, where, err := c.serveToken(ctx)
	if err != nil {
		return nil, "", err
	}
	if err := c.shareRemote(ctx); err != nil {
		return nil, "", err
	}
	store, err := c.store(ctx)
	if err != nil {
		return nil, "", err
	}
	c.log = slog.New(slog.NewTextHandler(c.errOut, nil))
	c.events = []agent.Observer{eventlog.Slog(c.log)}
	c.metrics = metrics.New()
	profiles, err := c.servedProfiles(ctx)
	if err != nil {
		return nil, "", err
	}
	app, err := appFiles(c.conf().Serve.App)
	if err != nil {
		return nil, "", err
	}
	arts, err := c.appArtifacts()
	if err != nil {
		return nil, "", err
	}
	peers, err := c.servePeers()
	if err != nil {
		return nil, "", err
	}
	srv, err := serve.New(serve.Config{Token: token, Store: store, MaxRuns: c.conf().Serve.maxRuns(), Profiles: profiles, App: app, Voice: c.voice(ctx), Artifacts: arts, Container: c.inContainer(), Peers: peers})
	if err != nil {
		return nil, "", err
	}
	srv.Mount("GET /metrics", c.metrics.Handler()) // behind the token: the counts tell what the agents do
	c.mountSettings(ctx, srv)
	c.mountState(ctx, srv)
	c.mountSetup(ctx, srv)
	return srv, where, nil
}

// servedProfiles are "default" (the flags and environment) and every
// profile of ringil.yaml.
func (c *cli) servedProfiles(ctx *orpheus.Context) ([]serve.Profile, error) {
	roles := c.conf().Profiles
	if p := ctx.GetGlobalFlagString("profile"); p != "" && p != "default" {
		if _, ok := roles[p]; !ok {
			return nil, errors.New(ErrUsage, "no profile "+p+" in "+c.configName())
		}
	}
	out := []serve.Profile{{Name: "default", Description: "the agent of ringil run", New: c.runInstance(ctx, "default")}}
	if c.settings != nil {
		served := map[string]bool{"default": true}
		for name := range roles {
			served[name] = true
		}
		c.settings.served.Store(&served)
	}
	for _, name := range names(roles) {
		out = append(out, serve.Profile{Name: name, Description: roles[name].Description, New: c.runInstance(ctx, name)})
	}
	return out, nil
}

// servedRole is profile name as ringil.yaml has it now; nil for default.
func (c *cli) servedRole(name string) (*profile, error) {
	if name == "default" {
		return nil, nil
	}
	p, ok := c.conf().Profiles[name]
	if !ok {
		return nil, errors.New(ErrUsage, "profile "+name+" is no longer in "+c.configName()+" (restart serve to drop it)")
	}
	return &p, nil
}

func a2aProfile(ctx *orpheus.Context) string {
	if p := ctx.GetGlobalFlagString("profile"); p != "" {
		return p
	}
	return "default"
}

// runInstance builds a run's agent: a CLI of its own, so that its crew,
// notebooks, task list and approvals are the run's alone. The role is
// read from ringil.yaml as it is now, so that an edit applies to the
// next run.
func (c *cli) runInstance(ctx *orpheus.Context, name string) func(context.Context) (serve.Instance, error) {
	return func(run context.Context) (serve.Instance, error) {
		r := c.forRun()
		r.settings = c.settings.pinned() // the whole run on one revision
		role, err := r.servedRole(name)
		if err != nil {
			return serve.Instance{}, err
		}
		if c.log != nil {
			r.events = []agent.Observer{eventlog.Slog(c.log.With("config", r.conf().shortDigest()))}
		}
		if c.metrics != nil { // a slice of its own: c.events is every run's
			r.events = slices.Concat(r.events, []agent.Observer{c.metrics.Observer(name)})
		}
		r.runConversation = serve.ConversationOf(run)
		r.setUpTrigger(serve.SourceOf(run))
		r.automating = serve.SourceOf(run) == channel.Automate
		r.inbox = c.inboxOf(r.runConversation)
		r.role, r.roleName = role, name
		a, err := r.agent(ctx)
		if err != nil {
			r.shutdown()
			return serve.Instance{}, err
		}
		return serve.Instance{Agent: a, Crew: r.crew, Close: r.shutdown}, nil
	}
}

// forRun is a CLI for one run of the server: the same settings and shared
// connections, nothing of another run, no terminal.
func (c *cli) forRun() *cli {
	return &cli{in: strings.NewReader(""), out: io.Discard, errOut: io.Discard, getenv: c.getenv,
		newProvider: c.newProvider, newAnthropic: c.newAnthropic, turnContext: c.turnContext, configDir: c.configDir, dataDir: c.dataDir, newEmbedder: c.newEmbedder,
		newSandbox: c.newSandbox, cwd: c.cwd, home: c.home, server: c.server, remote: c.remote, shared: true, events: c.events,
		triggers: c.triggers, bridge: c.bridge, slackBridge: c.slackBridge, board: c.board, settings: c.settings, secrets: c.secrets,
		signIns: c.signIns, chatgptIssuer: c.chatgptIssuer, chatgptBase: c.chatgptBase, prices: c.prices,
		traces: c.traces, traceWarned: c.traceWarned}
}

// exposeA2A offers a profile (--profile, else default) to other agents, at /a2a on the
// same listener, with the same token.
func (c *cli) exposeA2A(srv *serve.Server, profile, addr string) error {
	h, err := a2a.Expose(srv, profile, a2a.Card{Name: "ringil", Description: "A general agent: answers, reads files, uses its tools.", Version: version, URL: "http://" + addr + a2a.Path})
	if err != nil {
		return err
	}
	srv.Mount(a2a.Path, h)
	srv.Mount("/.well-known/", h)
	c.note("A2A agent at http://" + addr + a2a.Path)
	return nil
}

// shareRemote connects the MCP servers and remote agents once, for every
// run.
func (c *cli) shareRemote(ctx *orpheus.Context) error {
	remote, err := c.remoteTools(ctx)
	if err != nil {
		return err
	}
	c.remote, c.shared = remote, true
	return nil
}

// serveTokenName is the secret that sets serve's bearer token.
const serveTokenName = "RINGIL_SERVE_TOKEN" // #nosec G101 -- the secret's name, not its value

// serveToken is RINGIL_SERVE_TOKEN (the store's, or the environment's), or
// the token kept in the config directory, made the first time.
func (c *cli) serveToken(ctx *orpheus.Context) (token, where string, err error) {
	// From the store alone when there is one: a planted token would be
	// known to whoever planted it (audit 23/11).
	res, err := c.resolver(ctx)
	if err != nil {
		return "", "", err
	}
	if t, found, err := secrets.Lookup(ctx.Context(), storeOnly(res), serveTokenName); err != nil || found && t != "" {
		return t, "from " + serveTokenName, err
	}
	return c.keptServeToken()
}

// keptServeToken is the token kept in the config directory, made the first
// time.
func (c *cli) keptServeToken() (token, where string, err error) {
	base, err := c.configDir()
	if err != nil {
		return "", "", errors.Wrap(err, ErrUsage, "no user config directory; set RINGIL_SERVE_TOKEN")
	}
	dir := filepath.Join(base, "ringil")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", errors.Wrap(err, ErrUsage, "cannot create "+dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", "", errors.Wrap(err, ErrUsage, "cannot open "+dir)
	}
	defer closeQuietly(root)
	path := filepath.Join(dir, tokenFile)
	if info, err := root.Lstat(tokenFile); err == nil {
		t, err := keptToken(root, info, path)
		return t, "in " + path, err
	}
	token, err = newToken(root)
	if stderrors.Is(err, fs.ErrExist) {
		// Another serve made it a moment ago: read theirs once written.
		return awaitToken(root, path)
	}
	if err != nil {
		return "", "", errors.Wrap(err, ErrUsage, "cannot create "+path)
	}
	return token, "in " + path + " (new)", nil
}

// awaitToken reads a token file another process is writing.
func awaitToken(root *os.Root, path string) (string, string, error) {
	var err error
	for range 20 {
		var info os.FileInfo
		if info, err = root.Lstat(tokenFile); err == nil {
			var t string
			if t, err = keptToken(root, info, path); err == nil {
				return t, "in " + path, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", "", err
}

// keptToken reads the token file: regular, the user's, readable by no one
// else, 32 characters or more.
func keptToken(root *os.Root, info os.FileInfo, path string) (string, error) {
	bad := errors.New(ErrUsage, path+" is not a private file with a token of 32 characters or more; remove it to make a new one")
	if !info.Mode().IsRegular() || !private(info) || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return "", bad
	}
	data, err := root.ReadFile(tokenFile)
	if t := strings.TrimSpace(string(data)); err == nil && len(t) >= 32 {
		return t, nil
	}
	return "", bad
}

// newToken makes a token and publishes it whole: written to a file of
// its own, then linked at its name, which fails if another serve
// published one first. A reader never sees it half written.
func newToken(root *os.Root) (string, error) {
	token := rand.Text() + rand.Text()
	tmp := "." + tokenFile + "." + strings.ToLower(rand.Text()[:8]) + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Remove(tmp) }()
	_, err = f.WriteString(token + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return token, root.Link(tmp, tokenFile)
}

func closeQuietly(c io.Closer) {
	if err := c.Close(); err != nil {
		return
	}
}

// appFiles is the app's folder, when serve.app names one: it must hold
// the app's index.html.
func appFiles(dir string) (fs.FS, error) {
	if dir == "" {
		return nil, nil
	}
	if info, err := os.Stat(filepath.Join(dir, "index.html")); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New(ErrUsage, "serve.app: "+dir+" holds no index.html (the app's build)")
	}
	return os.DirFS(dir), nil
}

// voice is the app's speech to text and text to speech: the models of
// transcribe: and speech: as they are when it asks.
func (c *cli) voice(ctx *orpheus.Context) serve.Voice {
	return serve.Voice{
		Transcriber: func(context.Context) (agent.Transcriber, error) {
			res, err := c.resolver(ctx)
			if err != nil {
				return nil, err
			}
			return c.transcriber(ctx, res)
		},
		Synthesizer: func(context.Context) (agent.Synthesizer, error) {
			res, err := c.resolver(ctx)
			if err != nil {
				return nil, err
			}
			return c.synthesizer(ctx, res)
		},
	}
}

// appLink tells how to open the app: its sign-in link in a terminal; in a
// log (systemd's journal, a file) the link would be readable by others
// for its ten minutes, so there only how to ask one with the token.
func (c *cli) appLink(ctx *orpheus.Context, srv *serve.Server, addr string) {
	if c.conf().Serve.App == "" {
		return
	}
	terminal := c.logIsTerminal()
	link := "http://" + loopbackOf(addr) + srv.SignInPath()
	switch {
	case c.wantsBrowser(ctx, terminal) && c.openURL(link) == nil:
		c.note("app: opened in the browser; if it did not appear: " + link + " (once, within 10 minutes)")
	case terminal:
		c.note("app: " + link + " (opens it once, within 10 minutes)")
	default:
		c.note("app: http://" + loopbackOf(addr) + "/ ; its sign-in link: ringil serve link (in a container: docker exec <name> ringil serve link)")
	}
}

// serveListener listens where serve does: on loopback, or anywhere in a
// container. Serve checks it again; here before anything starts (a job).
func (c *cli) serveListener() (net.Listener, error) {
	listen := c.serveListen()
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "cannot listen on "+listen)
	}
	if addr, ok := l.Addr().(*net.TCPAddr); !ok || (!addr.IP.IsLoopback() && !c.inContainer()) {
		closeQuietly(l)
		return nil, errors.New(ErrUsage, "serve listens on loopback only (127.0.0.1 or ::1), not "+listen+"; in a container set serve.container (or RINGIL_CONTAINER=1)")
	}
	return l, nil
}

// inContainer: serve runs in a container: serve.container, or
// RINGIL_CONTAINER=1 (as the image sets) inside one, never on a desktop
// whose environment happens to carry it.
func (c *cli) inContainer() bool {
	v := strings.TrimSpace(c.getenv("RINGIL_CONTAINER"))
	return c.conf().Serve.Container || ((v == "1" || strings.EqualFold(v, "true")) && c.inImage != nil && c.inImage())
}

// inImage says whether the process runs in a container, by the marks
// Docker and Podman leave.
func inImage() bool {
	for _, mark := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(mark); err == nil {
			return true
		}
	}
	return false
}

// servePeers are the networks serve answers in a container: serve.peers,
// else the container's default gateway (the host).
func (c *cli) servePeers() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, p := range c.conf().Serve.Peers {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(p))
		if err != nil {
			return nil, errors.New(ErrUsage, "serve.peers: "+p+" is not a network such as 172.17.0.1/32")
		}
		out = append(out, prefix)
	}
	if len(out) == 0 {
		if gw, ok := defaultGateway(); ok {
			out = append(out, netip.PrefixFrom(gw, gw.BitLen()))
		}
	}
	return out, nil
}

// defaultGateway is the IPv4 default gateway from /proc/net/route (a
// Linux container), which is how the host reaches the container.
func defaultGateway() (netip.Addr, bool) {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return netip.Addr{}, false
	}
	return gatewayOf(string(data))
}

// gatewayOf reads the route table's default route: destination 0, its
// gateway in little-endian hex.
func gatewayOf(table string) (netip.Addr, bool) {
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		raw, err := hex.DecodeString(f[2])
		if err != nil || len(raw) != 4 || (raw[0]|raw[1]|raw[2]|raw[3]) == 0 {
			continue
		}
		return netip.AddrFrom4([4]byte{raw[3], raw[2], raw[1], raw[0]}), true // little-endian
	}
	return netip.Addr{}, false
}

// serveListen is where serve listens: ringil.yaml's, else the default of
// where it runs.
func (c *cli) serveListen() string {
	if c.conf().Serve.Listen == "" && c.inContainer() {
		return containerListen
	}
	return c.conf().Serve.listen()
}

// cmdServeLink asks the serve running here for a sign-in link of its app
// and prints it: for a serve whose log is not a terminal (a service, a
// container: docker exec ringil ringil serve link).
func (c *cli) cmdServeLink(ctx *orpheus.Context) error {
	token, _, err := c.serveToken(ctx)
	if err != nil {
		return err
	}
	addr := localOf(c.serveListen()) // always this machine's loopback: the token goes nowhere else
	req, err := http.NewRequestWithContext(ctx.Context(), http.MethodPost, "http://"+addr+"/v1/signin-links", nil)
	if err != nil {
		return errors.Wrap(err, ErrUsage, "bad serve address "+addr)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- serve on this machine's loopback
	if err != nil {
		return errors.Wrap(err, ErrUsage, "no serve answers on "+addr+": start it with ringil serve")
	}
	defer closeQuietly(resp.Body)
	var out struct {
		Path string `json:"path"`
	}
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out) != nil || !strings.HasPrefix(out.Path, "/signin?") {
		return errors.New(ErrUsage, "serve gave no sign-in link (status "+strconv.Itoa(resp.StatusCode)+"): is serve.app set?")
	}
	return c.line("http://" + addr + out.Path + "  (opens the app once, within 10 minutes)")
}

// localOf is serve's port on this machine's loopback, whatever host
// listen names.
func localOf(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		port = "8765"
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// loopbackOf is listen as this machine reaches it: an address on every
// interface is reached on 127.0.0.1.
func loopbackOf(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func (c *cli) peersText() string {
	peers, _ := c.servePeers()
	if len(peers) == 0 {
		return "no gateway found: the loopback only"
	}
	out := make([]string, len(peers))
	for i, p := range peers {
		out[i] = p.String()
	}
	return strings.Join(out, ", ")
}

// logIsTerminal: serve's log is a terminal, which a person reads.
func (c *cli) logIsTerminal() bool {
	f, ok := c.errOut.(*os.File)
	return ok && term.IsTerminal(int(f.Fd())) // #nosec G115 -- a file descriptor
}

// wantsBrowser: serve opens the app in the browser: started from a
// terminal, or with --open (an installer); never in a container or with
// --no-browser.
func (c *cli) wantsBrowser(ctx *orpheus.Context, terminal bool) bool {
	return !c.inContainer() && !c.flagBool(ctx, "no-browser") && (terminal || c.flagBool(ctx, "open"))
}
