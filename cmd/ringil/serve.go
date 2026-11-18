// serve.go: the agent behind a local HTTP API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

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
	token, where, err := c.serveToken()
	if err != nil {
		closeQuietly(l)
		return err
	}
	if err := c.shareRemote(ctx); err != nil {
		closeQuietly(l)
		return err
	}
	store, err := c.store(ctx)
	if err != nil {
		closeQuietly(l)
		return err
	}
	c.events = []agent.Observer{eventlog.Slog(slog.New(slog.NewTextHandler(c.errOut, nil)))}
	srv, err := serve.New(serve.Config{Token: token, Store: store, MaxRuns: ctx.GetFlagInt("max-runs"),
		Profiles: []serve.Profile{{Name: "default", Description: "the agent of ringil run", New: c.runInstance(ctx)}}})
	if err != nil {
		closeQuietly(l)
		return err
	}
	c.server = srv
	defer srv.Close()
	if ctx.GetFlagBool("a2a") {
		if err := c.exposeA2A(srv, l.Addr().String()); err != nil {
			return err
		}
	}
	run, stop := signal.NotifyContext(ctx.Context(), os.Interrupt)
	defer stop()
	c.note("serving on http://" + l.Addr().String() + "/v1/ ; bearer token " + where)
	return srv.Serve(run, l)
}

// runInstance builds a run's agent: a CLI of its own, so that its crew,
// notebooks, task list and approvals are the run's alone.
func (c *cli) runInstance(ctx *orpheus.Context) func(context.Context) (serve.Instance, error) {
	return func(context.Context) (serve.Instance, error) {
		r := c.forRun()
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

// exposeA2A offers the default profile to other agents, at /a2a on the
// same listener, with the same token.
func (c *cli) exposeA2A(srv *serve.Server, addr string) error {
	h, err := a2a.Expose(srv, "default", a2a.Card{Name: "ringil", Description: "A general agent: answers, reads files, uses its tools.", Version: version, URL: "http://" + addr + a2a.Path})
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
	if err != nil {
		return "", "", errors.Wrap(err, ErrUsage, "cannot create "+path)
	}
	return token, "in " + path + " (new)", nil
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

// newToken makes a token and keeps it in a new private file.
func newToken(root *os.Root) (string, error) {
	token := rand.Text() + rand.Text()
	f, err := root.OpenFile(tokenFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(token + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return token, err
}

func closeQuietly(c io.Closer) {
	if err := c.Close(); err != nil {
		return
	}
}
