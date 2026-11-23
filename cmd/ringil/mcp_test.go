// mcp_test.go: the MCP configuration of the terminal agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/sandbox"
	"github.com/agilira/ringil-agent-sdk/secrets"
)

func TestHeaderReferencesResolvePerRequest(t *testing.T) {
	vals := map[string]string{"KEY": "k1"}
	res := secrets.Map(vals)
	h := headerFunc(map[string]string{"X-Api-Key": "${KEY}", "Authorization": "Bearer ${KEY}"}, res)
	got, err := h(context.Background())
	if err != nil || got.Get("X-Api-Key") != "k1" || got.Get("Authorization") != "Bearer k1" {
		t.Fatalf("headers %v %v", got, err)
	}
	vals["KEY"] = "k2"
	if got, _ := h(context.Background()); got.Get("X-Api-Key") != "k2" {
		t.Fatalf("rotated key not used: %v", got)
	}
	if _, err := headerFunc(map[string]string{"K": "${MISSING}"}, res)(context.Background()); err == nil {
		t.Fatal("missing secret accepted")
	}
}

func TestServerEntryChecks(t *testing.T) {
	h := newHarness(t, "")
	res := secrets.Map(map[string]string{})
	for _, e := range []mcpEntry{{Type: "ws", URL: "https://x"}, {URL: "https://x", Command: "y"}} {
		if _, err := h.c.serverOf(context.Background(), "s", e, res); !errors.HasCode(err, ErrUsage) {
			t.Errorf("%+v: %v", e, err)
		}
	}
}

// Audit 18/11: a remote agent named like a built-in tool stopped the CLI.
func TestClashingRemoteToolIsLeftOut(t *testing.T) {
	h := newHarness(t, "")
	reg, err := builtinTools()
	if err != nil {
		t.Fatal(err)
	}
	clash := reg.Specs()[0].Name
	tools, err := builtinTools()
	if err != nil {
		t.Fatal(err)
	}
	got := h.c.addEach(reg, []agent.Tool{byNameTool(t, tools, clash)})
	if len(got.Specs()) != len(reg.Specs()) || !strings.Contains(h.err.String(), "left out") {
		t.Fatalf("clash not reported: %q", h.err)
	}
}

func byNameTool(t *testing.T, reg agent.Registry, name string) agent.Tool {
	t.Helper()
	for _, s := range reg.Specs() {
		if s.Name == name {
			return agent.NewTool(name, "dup", func(context.Context, struct{}) (string, error) { return "", nil })
		}
	}
	t.Fatalf("no %s", name)
	return nil
}

// 23/11: a local MCP server runs as a plain process unless sandboxed; a
// sandboxed one's network is the egress proxy, for its allowed domains
// alone; the old network key is refused, saying where it went.
func TestMCPServersSandboxedOnRequest(t *testing.T) {
	h := newHarness(t, "")
	h.c.newSandbox = func() (sandbox.Sandbox, error) { return &stubSandbox{}, nil }
	ctx := context.Background()
	srv, err := h.c.serverOf(ctx, "files", mcpEntry{Command: "mcp-files", Sandbox: true, AllowDomains: []string{"api.example.com"}}, secrets.Map{})
	if err != nil || srv.Sandbox == nil || srv.Proxy == nil {
		t.Fatalf("%+v %v", srv, err)
	}
	srv, err = h.c.serverOf(ctx, "plain", mcpEntry{Command: "mcp-files"}, secrets.Map{})
	if err != nil || srv.Sandbox != nil {
		t.Fatalf("%+v %v", srv, err)
	}
}

func TestOldMCPSandboxKeysRefused(t *testing.T) {
	yes := true
	for _, e := range []mcpEntry{{Command: "x", OldNetwork: &yes}, {Command: "x", AllowDomains: []string{"a.com"}}, {Command: "x", Mounts: []string{"/x"}}} {
		if e.check() == nil {
			t.Errorf("accepted %+v", e)
		}
	}
}
