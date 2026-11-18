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
	"strconv"
	"strings"
	"sync"
	"time"

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
	name  string
	srv   Server
	allow map[string]bool
	stop  func() error

	// life ends with Close: calls in flight are cancelled, so Close never
	// waits behind a server that stopped answering.
	life   context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	session *sdk.ClientSession
	tools   []agent.Tool
}

// Bounds on what a server can make the client hold.
const (
	maxTools        = 1000
	maxSchema       = 64 << 10
	defaultOutput   = 256 << 10
	defaultCallTime = 5 * time.Minute
	refreshTime     = 30 * time.Second
)

// Connect starts or reaches the server and lists its tools.
func Connect(ctx context.Context, srv Server) (*Client, error) {
	if err := srv.validate(); err != nil {
		return nil, err
	}
	transport, stop, err := srv.transport(ctx)
	if err != nil {
		return nil, err
	}
	return connect(ctx, srv, transport, stop)
}

func connect(ctx context.Context, srv Server, transport sdk.Transport, stop func() error) (*Client, error) {
	life, cancel := context.WithCancel(context.Background())
	c := &Client{name: srv.Name, srv: srv, stop: stop, allow: set(srv.Tools), life: life, cancel: cancel}
	client := sdk.NewClient(&sdk.Implementation{Name: "ringil-agent-sdk", Version: "0"}, &sdk.ClientOptions{
		ToolListChangedHandler: c.onListChanged,
	})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, c.fail(errors.Wrap(err, ErrConnect, "cannot connect to MCP server "+srv.Name+": "+err.Error()))
	}
	c.mu.Lock()
	c.session = session
	c.mu.Unlock()
	if err := c.refresh(ctx, session); err != nil {
		return nil, c.fail(err)
	}
	return c, nil
}

// onListChanged re-reads the tools. A notice that arrives before Connect
// has finished is ignored: the session is not usable yet, and Connect
// reads the tools itself right after.
func (c *Client) onListChanged(context.Context, *sdk.ToolListChangedRequest) {
	session := c.current()
	if session == nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.life, refreshTime)
	defer cancel()
	if err := c.refresh(ctx, session); err != nil {
		return // keep the tools we have; the next call reports trouble
	}
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

func (c *Client) current() *sdk.ClientSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// Close cancels calls in flight, ends the session and stops a local
// server. It may be called more than once.
func (c *Client) Close() error {
	c.cancel()
	var err error
	if s := c.current(); s != nil {
		err = s.Close()
	}
	if c.stop != nil {
		if serr := c.stop(); err == nil {
			err = serr
		}
	}
	return err
}

func (c *Client) refresh(ctx context.Context, session *sdk.ClientSession) error {
	var tools []agent.Tool
	names := map[string]bool{}
	count := 0
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			return errors.Wrap(err, ErrConnect, "cannot list the tools of MCP server "+c.name+": "+err.Error())
		}
		if count++; count > maxTools {
			break
		}
		if len(c.allow) > 0 && !c.allow[t.Name] {
			continue
		}
		if tool, ok := c.adapt(t, names); ok {
			tools = append(tools, tool)
		}
	}
	c.mu.Lock()
	c.tools = tools
	c.mu.Unlock()
	return nil
}

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// toolName joins server and tool into a name every provider accepts:
// the server part has no "_" (so "a" + "b__c" and "a__b" + "c" differ),
// and a name already taken gets a numbered suffix instead of being lost.
func toolName(server, tool string, taken map[string]bool) string {
	base := strings.ReplaceAll(server, "_", "-") + "__" + unsafeChars.ReplaceAllString(tool, "_")
	name := clip(base, 64)
	for i := 2; taken[name]; i++ {
		suffix := "_" + strconv.Itoa(i)
		name = clip(base, 64-len(suffix)) + suffix
	}
	taken[name] = true
	return name
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func set(names []string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[strings.TrimSpace(n)] = true
	}
	return m
}
