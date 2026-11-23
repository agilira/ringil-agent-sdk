// serve.go: the agent behind a local HTTP API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"crypto/rand"
	stderrors "errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/a2a"
	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/eventlog"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
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
	listen := c.conf().Serve.listen()
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return errors.Wrap(err, ErrUsage, "cannot listen on "+listen)
	}
	if addr, ok := l.Addr().(*net.TCPAddr); !ok || !addr.IP.IsLoopback() {
		// Checked again by Serve; here before anything starts (a job).
		closeQuietly(l)
		return errors.New(ErrUsage, "serve listens on loopback only (127.0.0.1 or ::1), not "+listen)
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
	stopAll, err := c.startBackground(ctx, run, srv)
	if err != nil {
		return err
	}
	defer stopAll()
	c.note("serving on http://" + l.Addr().String() + "/v1/ ; bearer token " + where)
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
	srv, err := serve.New(serve.Config{Token: token, Store: store, MaxRuns: c.conf().Serve.maxRuns(), Profiles: profiles})
	if err != nil {
		return nil, "", err
	}
	srv.Mount("GET /metrics", c.metrics.Handler()) // behind the token: the counts tell what the agents do
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
		newProvider: c.newProvider, newAnthropic: c.newAnthropic, turnContext: c.turnContext, configDir: c.configDir, newEmbedder: c.newEmbedder,
		newSandbox: c.newSandbox, cwd: c.cwd, home: c.home, server: c.server, remote: c.remote, shared: true, events: c.events,
		triggers: c.triggers, bridge: c.bridge, board: c.board, settings: c.settings, secrets: c.secrets,
		signIns: c.signIns, chatgptIssuer: c.chatgptIssuer, chatgptBase: c.chatgptBase}
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
	if !info.Mode().IsRegular() || lockfile.CheckPrivate(info) != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
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
