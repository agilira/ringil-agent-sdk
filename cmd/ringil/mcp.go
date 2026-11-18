// mcp.go: MCP servers from the user's mcp.json
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
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
type mcpFile struct {
	Servers map[string]mcpEntry `json:"mcpServers"`
}

// mcpEntry is one server. Values may hold ${NAME}, resolved from the
// environment and the --env-file when the server starts (env) or on
// every request (headers), never stored expanded.
type mcpEntry struct {
	Type    string            `json:"type,omitempty"` // "stdio" (default with command) or "http"
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Tools   []string          `json:"tools,omitempty"`
	// Sandbox runs a local server isolated (Linux), with Network and
	// read-only Mounts; off by default, as the user chose the command.
	Sandbox bool     `json:"sandbox,omitempty"`
	Network bool     `json:"network,omitempty"`
	Mounts  []string `json:"mounts,omitempty"`
}

func (c *cli) mcpPath(ctx *orpheus.Context) (string, error) {
	if p := ctx.GetGlobalFlagString("mcp-config"); p != "" {
		return p, nil
	}
	base, err := c.configDir()
	if err != nil {
		return "", nil
	}
	return filepath.Join(base, "ringil", "mcp.json"), nil
}

// loadMCP reads the configuration: a file of the user's, not writable by
// others, since it decides what runs. A missing default file is no
// configuration.
func loadMCP(path string, explicit bool) (map[string]mcpEntry, error) {
	data, err := readPrivate(path)
	if os.IsNotExist(err) && !explicit {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "cannot read the MCP configuration "+path)
	}
	var f mcpFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, errors.Wrap(err, ErrUsage, "the MCP configuration is not valid JSON of the expected shape")
	}
	return f.Servers, nil
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
		return nil, errors.New(ErrUsage, "the MCP configuration must be a regular file of yours, not writable by others")
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
	path, err := c.mcpPath(ctx)
	if err != nil || path == "" {
		return nil, err
	}
	entries, err := loadMCP(path, ctx.GetGlobalFlagString("mcp-config") != "")
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	var tools []agent.Tool
	for _, name := range sortedKeys(entries) {
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
	srv := mcp.Server{Name: name, URL: e.URL, Tools: e.Tools}
	if e.URL != "" || e.Type == "http" {
		srv.Headers = headerFunc(e.Headers, res)
		return srv, nil
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

// shutdown closes the MCP servers started for this command.
func (c *cli) shutdown() {
	for _, closeFn := range c.closers {
		if err := closeFn(); err != nil {
			c.note("closing an MCP server: " + err.Error())
		}
	}
	c.closers = nil
}
