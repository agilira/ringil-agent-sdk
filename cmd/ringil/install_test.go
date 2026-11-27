// install_test.go: add_mcp_server saving into ringil.yaml
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/mcp"
	"github.com/agilira/ringil-agent-sdk/secrets"
)

// fakeMCP is a remote MCP server on loopback with two tools.
func fakeMCP(t *testing.T) string {
	t.Helper()
	type in struct {
		Q string `json:"q"`
	}
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
		s := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "1"}, nil)
		for _, name := range []string{"search", "create_pod"} {
			sdk.AddTool(s, &sdk.Tool{Name: name}, func(context.Context, *sdk.CallToolRequest, in) (*sdk.CallToolResult, any, error) {
				return &sdk.CallToolResult{}, nil, nil
			})
		}
		return s
	}, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

type envResolver map[string]string

func (r envResolver) Lookup(_ context.Context, name string) (string, bool, error) {
	v, ok := r[name]
	return v, ok, nil
}

func addHarness(t *testing.T, fn func(c *cli, path string)) {
	t.Helper()
	h := newHarness(t, "")
	env(h, map[string]string{})
	path := writeConfig(t, h.dir, "models: [openai:m]\nmcp_servers:\n  old:\n    command: srv\n")
	loaded(t, h.c, nil, func(*orpheus.Context) error {
		fn(h.c, path)
		return nil
	})
}

func resolveWith(r envResolver) func() (secrets.Resolver, error) {
	return func() (secrets.Resolver, error) { return r, nil }
}

func TestAddMCPRemoteSavesAndLists(t *testing.T) {
	url := fakeMCP(t) + "/mcp"
	addHarness(t, func(c *cli, path string) {
		got, err := c.addMCP(context.Background(), mcp.Entry{Name: "runpod", URL: url, Source: "https://docs.runpod.io/mcp"}, resolveWith(nil))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got.Tools, ",") != "runpod__create_pod,runpod__search" || !got.NextSession || len(got.Missing) != 0 {
			t.Fatalf("added %+v", got)
		}
		text := readText(t, path)
		for _, want := range []string{"runpod:", "url: " + url, "source: https://docs.runpod.io/mcp", "oauth: {}", "old:"} {
			if !strings.Contains(text, want) {
				t.Fatalf("ringil.yaml lacks %q:\n%s", want, text)
			}
		}
	})
}

// In a session, the added server's tools join the agent's at once.
func TestAddMCPJoinsTheLiveSession(t *testing.T) {
	url := fakeMCP(t) + "/mcp"
	addHarness(t, func(c *cli, _ string) {
		c.live = agent.NewLiveRegistry(nil)
		got, err := c.addMCP(context.Background(), mcp.Entry{Name: "dw", URL: url}, resolveWith(nil))
		if err != nil || got.NextSession {
			t.Fatalf("added %+v, %v", got, err)
		}
		if !c.live.Has("dw__search") || !c.live.Has("dw__create_pod") {
			t.Fatalf("live tools %v", c.live.Specs())
		}
	})
}

func TestAddMCPMissingSecretSavesWithoutStarting(t *testing.T) {
	addHarness(t, func(c *cli, path string) {
		e := mcp.Entry{Name: "gh", Command: []string{"/nonexistent/srv", "--stdio"}, Env: map[string]string{"GITHUB_TOKEN": "${GITHUB_TOKEN}"}}
		got, err := c.addMCP(context.Background(), e, resolveWith(envResolver{}))
		if err != nil || strings.Join(got.Missing, ",") != "GITHUB_TOKEN" || len(got.Tools) != 0 {
			t.Fatalf("added %+v, %v", got, err)
		}
		text := readText(t, path)
		for _, want := range []string{"command: /nonexistent/srv", "--stdio", "GITHUB_TOKEN: ${GITHUB_TOKEN}"} {
			if !strings.Contains(text, want) {
				t.Fatalf("ringil.yaml lacks %q:\n%s", want, text)
			}
		}
		if strings.Contains(text, "oauth") {
			t.Fatalf("a local server got oauth:\n%s", text)
		}
	})
}

func TestAddMCPDoesNotStartTellsTheModel(t *testing.T) {
	addHarness(t, func(c *cli, path string) {
		_, err := c.addMCP(context.Background(), mcp.Entry{Name: "bad", Command: []string{"/nonexistent/srv"}}, resolveWith(envResolver{}))
		if msg := agent.ModelMessage(err); !strings.Contains(msg, "saved in ringil.yaml but did not start") {
			t.Fatalf("model sees %q", msg)
		}
		if !strings.Contains(readText(t, path), "bad:") {
			t.Fatal("not saved")
		}
	})
}

func TestAddMCPTakenName(t *testing.T) {
	addHarness(t, func(c *cli, path string) {
		before := readText(t, path)
		_, err := c.addMCP(context.Background(), mcp.Entry{Name: "old", Command: []string{"x"}}, resolveWith(nil))
		if !errors.HasCode(err, errMCPExists) || !strings.Contains(agent.ModelMessage(err), "already configured") {
			t.Fatalf("err %v", err)
		}
		if readText(t, path) != before {
			t.Fatal("ringil.yaml changed")
		}
	})
}

func TestInstallToolsOnlyForTheOwner(t *testing.T) {
	h := newHarness(t, "")
	env(h, map[string]string{})
	probe(t, h.c, nil, func(ctx *orpheus.Context) error {
		names := map[string]bool{}
		for _, tool := range h.c.installTools(ctx, nil) {
			names[tool.Spec().Name] = true
		}
		if !names["install_skill"] || !names["add_mcp_server"] {
			t.Errorf("owner tools %v", names)
		}
		h.c.inTrigger = true
		if got := h.c.installTools(ctx, nil); len(got) != 0 {
			t.Errorf("a trigger's run got %d install tools", len(got))
		}
		return nil
	})
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
