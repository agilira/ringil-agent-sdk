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
	"strings"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/a2a"
	"github.com/agilira/ringil-agent-sdk/eventlog"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
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
	listen := ctx.GetFlagString("listen")
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return errors.Wrap(err, ErrUsage, "cannot listen on "+listen)
	}
	if addr, ok := l.Addr().(*net.TCPAddr); !ok || !addr.IP.IsLoopback() {
		// Checked again by Serve; here before anything starts (a job).
		closeQuietly(l)
		return errors.New(ErrUsage, "serve listens on loopback only (127.0.0.1 or ::1), not "+listen)
	}
	srv, where, err := c.newServer(ctx)
	if err != nil {
		closeQuietly(l)
		return err
	}
	c.server = srv
	defer srv.Close()
	if ctx.GetFlagBool("a2a") {
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
// --no-triggers) and the chat apps. The function returned stops
// them.
func (c *cli) startBackground(ctx *orpheus.Context, run context.Context, srv *serve.Server) (func(), error) {
	stopChannels, err := c.startChannels(ctx, run, srv)
	if err != nil {
		return nil, err
	}
	stopTriggers := func() {}
	if !ctx.GetFlagBool("no-triggers") {
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
	token, where, err := c.serveToken()
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
	c.events = []agent.Observer{eventlog.Slog(slog.New(slog.NewTextHandler(c.errOut, nil)))}
	profiles, err := c.servedProfiles(ctx)
	if err != nil {
		return nil, "", err
	}
	srv, err := serve.New(serve.Config{Token: token, Store: store, MaxRuns: ctx.GetFlagInt("max-runs"), Profiles: profiles})
	return srv, where, err
}

// servedProfiles are "default" (the flags and environment) and every
// role of the profiles file.
func (c *cli) servedProfiles(ctx *orpheus.Context) ([]serve.Profile, error) {
	roles, err := c.loadProfiles(ctx)
	if err != nil {
		return nil, err
	}
	if p := ctx.GetGlobalFlagString("profile"); p != "" && p != "default" {
		if _, ok := roles[p]; !ok {
			return nil, errors.New(ErrUsage, "no profile "+p+" in "+c.profilesPath(ctx))
		}
	}
	out := []serve.Profile{{Name: "default", Description: "the agent of ringil run", New: c.runInstance(ctx, nil)}}
	for _, name := range names(roles) {
		role := roles[name]
		out = append(out, serve.Profile{Name: name, Description: role.Description, New: c.runInstance(ctx, &role)})
	}
	return out, nil
}

func a2aProfile(ctx *orpheus.Context) string {
	if p := ctx.GetGlobalFlagString("profile"); p != "" {
		return p
	}
	return "default"
}

// runInstance builds a run's agent: a CLI of its own, so that its crew,
// notebooks, task list and approvals are the run's alone.
func (c *cli) runInstance(ctx *orpheus.Context, role *profile) func(context.Context) (serve.Instance, error) {
	return func(run context.Context) (serve.Instance, error) {
		r := c.forRun()
		r.runConversation = serve.ConversationOf(run)
		r.setUpTrigger(serve.SourceOf(run))
		r.inbox = c.inboxOf(r.runConversation)
		r.role = role
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
		newProvider: c.newProvider, turnContext: c.turnContext, configDir: c.configDir, newEmbedder: c.newEmbedder,
		newSandbox: c.newSandbox, cwd: c.cwd, home: c.home, server: c.server, remote: c.remote, shared: true, events: c.events}
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

// serveToken is RINGIL_SERVE_TOKEN, or the token kept in the config
// directory, made the first time.
func (c *cli) serveToken() (token, where string, err error) {
	if t := c.getenv("RINGIL_SERVE_TOKEN"); t != "" {
		return t, "from RINGIL_SERVE_TOKEN", nil
	}
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
