// mcp.go: MCP servers, from the mcp_servers of ringil.yaml
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
	"github.com/agilira/ringil-agent-sdk/mcp"
	"github.com/agilira/ringil-agent-sdk/sandbox"
	"github.com/agilira/ringil-agent-sdk/secrets"
)

// mcpFile is the usual MCP configuration shape: {"mcpServers": {...}}.
// mcpEntry is one server. Values may hold ${NAME}, resolved from the
// environment and the --env-file when the server starts (env) or on
// every request (headers), never stored expanded.
type mcpEntry struct {
	Type    string            `argus:"type"` // "stdio" (default with command) or "http"
	Command string            `argus:"command"`
	Args    []string          `argus:"args"`
	Env     map[string]string `argus:"env"`
	URL     string            `argus:"url"`
	Headers map[string]string `argus:"headers"`
	Tools   []string          `argus:"tools"`
	// Sandbox runs a local server isolated (Linux), with Network and
	// read-only Mounts; off by default, as the user chose the command.
	Sandbox bool     `argus:"sandbox"`
	Network bool     `argus:"network"`
	Mounts  []string `argus:"mounts"`
	// Disabled keeps the entry without starting it.
	Disabled bool `argus:"disabled"`
	// TrustAnnotations lets the server's read-only hints skip the
	// confirmation; by default every MCP call is confirmed.
	TrustAnnotations bool `argus:"trust_annotations"`
	// OAuth signs in to a remote server through the browser.
	OAuth *oauthEntry `argus:"oauth"`
}

// oauthEntry: empty registers the client dynamically; a registered
// client gives its ID, secret (${NAME}) and the callback port it was
// registered with.
type oauthEntry struct {
	ClientID     string `argus:"client_id"`
	ClientSecret string `argus:"client_secret"`
	Port         int    `argus:"port"`
}

// readPrivate reads a regular file of the user's, not writable by others,
// checking the file it opened rather than the path, and following no
// symbolic link at its name.
func readPrivate(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer closeRoot(root)
	f, err := root.OpenFile(filepath.Base(path), os.O_RDONLY|lockfile.NoFollow, 0)
	if err != nil {
		return nil, err
	}
	defer closeFile(f)
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || lockfile.CheckPrivate(info) != nil {
		return nil, errors.New(ErrUsage, "the configuration must be a regular file of yours, not writable by others")
	}
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

func closeRoot(r *os.Root) {
	if err := r.Close(); err != nil {
		return // read-only handle
	}
}

func closeFile(f *os.File) {
	if err := f.Close(); err != nil {
		return // read-only handle
	}
}

// connectMCP starts every configured server. A server that fails is
// reported and left out; the others still work.
func (c *cli) connectMCP(ctx *orpheus.Context) ([]agent.Tool, error) {
	entries := c.conf().MCPServers
	if len(entries) == 0 {
		return nil, nil
	}
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	var tools []agent.Tool
	for _, name := range sortedKeys(entries) {
		if entries[name].Disabled {
			continue
		}
		client, err := c.startServer(ctx.Context(), name, entries[name], res)
		if err != nil {
			c.note("MCP server " + name + " left out: " + err.Error())
			continue
		}
		c.closers = append(c.closers, client.Close)
		tools = append(tools, client.Tools()...)
	}
	return tools, nil
}

func (c *cli) startServer(ctx context.Context, name string, e mcpEntry, res secrets.Resolver) (*mcp.Client, error) {
	srv, err := c.serverOf(ctx, name, e, res)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return mcp.Connect(ctx, srv)
}

func (c *cli) serverOf(ctx context.Context, name string, e mcpEntry, res secrets.Resolver) (mcp.Server, error) {
	srv := mcp.Server{Name: name, URL: e.URL, Tools: e.Tools, TrustAnnotations: e.TrustAnnotations}
	if err := e.check(); err != nil {
		return srv, err
	}
	if e.URL != "" {
		srv.Headers = headerFunc(e.Headers, res)
		return c.withOAuth(ctx, srv, e.OAuth, res)
	}
	env, err := expandAll(ctx, e.Env, res)
	if err != nil {
		return srv, err
	}
	srv.Command, srv.Env = append([]string{e.Command}, e.Args...), env
	if e.Sandbox {
		return c.sandboxServer(srv, e)
	}
	return srv, nil
}

func (c *cli) sandboxServer(srv mcp.Server, e mcpEntry) (mcp.Server, error) {
	sb, err := c.newSandbox()
	if err != nil {
		return srv, err
	}
	srv.Sandbox = sb
	if e.Network {
		srv.Network = sandbox.IsolatedNetwork
	}
	for _, m := range e.Mounts {
		mount, ok := maskedMount(m)
		if !ok {
			return srv, errors.New(ErrUsage, "cannot mount "+m+" for the MCP server")
		}
		srv.Mounts = append(srv.Mounts, mount)
	}
	if base, err := c.configDir(); err == nil {
		srv.Protected = []string{filepath.Join(base, "ringil")}
	}
	return srv, nil
}

func (e mcpEntry) check() error {
	switch {
	case e.Type != "" && e.Type != "stdio" && e.Type != "http" && e.Type != "streamable-http":
		return errors.New(ErrUsage, "unknown server type "+e.Type)
	case e.URL != "" && e.Command != "":
		return errors.New(ErrUsage, "give either a command or a url, not both")
	}
	return nil
}

// withOAuth adds the browser sign-in; tokens are kept in the user's
// config directory, private, so the next run needs no new sign-in.
func (c *cli) withOAuth(ctx context.Context, srv mcp.Server, o *oauthEntry, res secrets.Resolver) (mcp.Server, error) {
	if o == nil {
		return srv, nil
	}
	secret, err := expandAll(ctx, map[string]string{"s": o.ClientSecret}, res)
	if err != nil {
		return srv, err
	}
	login := mcp.Login{ClientID: o.ClientID, ClientSecret: secret["s"], Port: o.Port, Show: c.showSignIn(srv.Name)}
	if base, err := c.configDir(); err == nil {
		login.Tokens = mcp.FileTokens(filepath.Join(base, "ringil", "oauth", srv.Name+".json"))
	}
	h, stop, err := mcp.NewLogin(login)
	if err != nil {
		return srv, err
	}
	c.closers = append(c.closers, stop)
	srv.OAuth = h
	return srv, nil
}

func (c *cli) showSignIn(name string) func(string) {
	return func(link string) {
		c.note("Sign in to MCP server " + name + " — open this link in your browser:")
		c.note(link)
	}
}

// headerFunc resolves ${NAME} in header values on every request, so
// rotated keys apply and no expanded secret is kept.
func headerFunc(raw map[string]string, res secrets.Resolver) func(context.Context) (http.Header, error) {
	if len(raw) == 0 {
		return nil
	}
	return func(ctx context.Context) (http.Header, error) {
		vals, err := expandAll(ctx, raw, res)
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		for k, v := range vals {
			h.Set(k, v)
		}
		return h, nil
	}
}

var reference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandAll(ctx context.Context, raw map[string]string, res secrets.Resolver) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var failed error
		out[k] = reference.ReplaceAllStringFunc(v, func(m string) string {
			name := reference.FindStringSubmatch(m)[1]
			val, found, err := secrets.Lookup(ctx, res, name)
			switch {
			case failed != nil:
			case err != nil:
				failed = errors.Wrap(err, ErrUsage, "cannot resolve ${"+name+"}")
			case !found:
				failed = errors.New(ErrUsage, "${"+name+"} is not set in the environment or the --env-file")
			}
			return val
		})
		if failed != nil {
			return nil, failed
		}
	}
	return out, nil
}

func sortedKeys(m map[string]mcpEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// shutdown releases what this command started, newest first: workers stop
// before the MCP servers and notebooks they use.
func (c *cli) shutdown() {
	for i := len(c.closers) - 1; i >= 0; i-- {
		if err := c.closers[i](); err != nil {
			c.note("cleanup at exit: " + err.Error())
		}
	}
	c.closers = nil
}
