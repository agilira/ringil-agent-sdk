// mcp.go: MCP servers as tools of an agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package mcp connects an agent to Model Context Protocol servers through
// the official Go SDK, and offers each server's tools as agent tools. It
// is a module of its own, so the agent core keeps no dependency on it.
//
// A local server runs as a command over stdio, in the sandbox when one is
// given; a remote server is reached over Streamable HTTP, HTTPS only
// (loopback excepted), with credentials from the host and OAuth through
// the SDK's handler. Redirects to another origin are not followed, so
// credentials go only where the host sent them.
package mcp

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"github.com/agilira/go-errors"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Error codes.
const (
	ErrConfig  errors.ErrorCode = "MCP_CONFIG"
	ErrConnect errors.ErrorCode = "MCP_CONNECT"
	ErrCall    errors.ErrorCode = "MCP_CALL"
)

// Client is a connection to one MCP server.
type Client struct {
	name    string
	session *sdk.ClientSession
	allow   map[string]bool
	stop    func() error

	mu    sync.Mutex
	tools []agent.Tool
}

// Connect starts or reaches the server and lists its tools.
func Connect(ctx context.Context, srv Server) (*Client, error) {
	if err := srv.validate(); err != nil {
		return nil, err
	}
	transport, stop, err := srv.transport(ctx)
	if err != nil {
		return nil, err
	}
	return connect(ctx, srv.Name, transport, stop, srv.Tools)
}

func connect(ctx context.Context, name string, transport sdk.Transport, stop func() error, allow []string) (*Client, error) {
	c := &Client{name: name, stop: stop, allow: set(allow)}
	client := sdk.NewClient(&sdk.Implementation{Name: "ringil-agent-sdk", Version: "0"}, &sdk.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, _ *sdk.ToolListChangedRequest) {
			if err := c.refresh(ctx); err != nil {
				return // keep the tools we have; the next call reports trouble
			}
		},
	})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, c.fail(errors.Wrap(err, ErrConnect, "cannot connect to MCP server "+name))
	}
	c.session = session
	if err := c.refresh(ctx); err != nil {
		return nil, c.fail(err)
	}
	return c, nil
}

func (c *Client) fail(err error) error {
	if cerr := c.Close(); cerr != nil {
		return errors.Wrap(err, ErrConnect, "MCP server failed and did not close").WithContext("close_error", cerr.Error())
	}
	return err
}

// Tools are the server's tools, named "<server>__<tool>". The list follows
// the server's change notifications; call Tools again to see changes.
func (c *Client) Tools() []agent.Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]agent.Tool(nil), c.tools...)
}

// Close ends the session and stops a local server.
func (c *Client) Close() error {
	var err error
	if c.session != nil {
		err = c.session.Close()
	}
	if c.stop != nil {
		if serr := c.stop(); err == nil {
			err = serr
		}
	}
	return err
}

func (c *Client) refresh(ctx context.Context) error {
	var tools []agent.Tool
	seen := map[string]bool{}
	for t, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return errors.Wrap(err, ErrConnect, "cannot list the tools of MCP server "+c.name)
		}
		if len(c.allow) > 0 && !c.allow[t.Name] {
			continue
		}
		tool, ok := c.adapt(t)
		if ok && !seen[tool.Spec().Name] {
			seen[tool.Spec().Name] = true
			tools = append(tools, tool)
		}
	}
	c.mu.Lock()
	c.tools = tools
	c.mu.Unlock()
	return nil
}

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// toolName joins server and tool into a name every provider accepts.
func toolName(server, tool string) string {
	n := server + "__" + unsafeChars.ReplaceAllString(tool, "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

func set(names []string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[strings.TrimSpace(n)] = true
	}
	return m
}
