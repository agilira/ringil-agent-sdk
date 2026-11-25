// add_test.go: checking and adding MCP servers the user asked for
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

func TestEntryCheckAccepts(t *testing.T) {
	for name, e := range map[string]Entry{
		"npx":         {Name: "github", Command: []string{"npx", "-y", "@modelcontextprotocol/server-github"}, Env: map[string]string{"GITHUB_TOKEN": "${GITHUB_TOKEN}", "LOG_LEVEL": "info"}},
		"uvx":         {Name: "time_srv", Command: []string{"uvx", "mcp-server-time", "--local-timezone=Europe/Rome"}},
		"docker":      {Name: "d-1", Command: []string{"docker", "run", "-i", "--rm", "ghcr.io/org/srv:1.2"}},
		"remote":      {Name: "linear", URL: "https://mcp.linear.app/mcp"},
		"bearer":      {Name: "r", URL: "https://x.example.com/mcp", Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"}},
		"bare ref":    {Name: "r", URL: "https://x.example.com/mcp", Headers: map[string]string{"X-Api-Key": "${API_KEY}"}},
		"loopback":    {Name: "local", URL: "http://127.0.0.1:8931/mcp"},
		"with spaces": {Name: "p", Command: []string{"/opt/My Server/bin/srv", "--name", "a b"}, Source: "https://github.com/o/r"},
	} {
		if err := e.Check(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestEntryCheckRefuses(t *testing.T) {
	long := strings.Repeat("a", maxValue+1)
	many := make([]string, maxArgs+1)
	for i := range many {
		many[i] = "a"
	}
	for name, e := range map[string]Entry{
		"no name":         {Command: []string{"x"}},
		"bad name":        {Name: "a b", Command: []string{"x"}},
		"long name":       {Name: strings.Repeat("a", 33), Command: []string{"x"}},
		"neither":         {Name: "a"},
		"both":            {Name: "a", Command: []string{"x"}, URL: "https://x.example.com"},
		"empty program":   {Name: "a", Command: []string{" ", "x"}},
		"http":            {Name: "a", URL: "http://x.example.com/mcp"},
		"credentials":     {Name: "a", URL: "https://u:p@x.example.com/mcp"},
		"not a url":       {Name: "a", URL: "x.example.com"},
		"control in arg":  {Name: "a", Command: []string{"npx", "a\nb"}},
		"bidi in arg":     {Name: "a", Command: []string{"npx", "safe\u202eexe.txt"}},
		"zero width":      {Name: "a", Command: []string{"np\u200bx"}},
		"long arg":        {Name: "a", Command: []string{"x", long}},
		"many args":       {Name: "a", Command: many},
		"bad env name":    {Name: "a", Command: []string{"x"}, Env: map[string]string{"A-B": "1"}},
		"env on remote":   {Name: "a", URL: "https://x.example.com", Env: map[string]string{"A": "1"}},
		"header on local": {Name: "a", Command: []string{"x"}, Headers: map[string]string{"A": "${A}"}},
		"key in clear":    {Name: "a", URL: "https://x.example.com", Headers: map[string]string{"Authorization": "Bearer sk-live-123"}},
		"ref plus text":   {Name: "a", URL: "https://x.example.com", Headers: map[string]string{"Authorization": "Bearer ${T} sk-123"}},
		"bad header name": {Name: "a", URL: "https://x.example.com", Headers: map[string]string{"X:Y": "${A}"}},
		"source control":  {Name: "a", Command: []string{"x"}, Source: "https://github.com/o/r\r\nX: y"},
	} {
		if err := e.Check(); !errors.HasCode(err, ErrConfig) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

func TestEntryReferences(t *testing.T) {
	e := Entry{Name: "a", Command: []string{"srv", "--key=${B_KEY}"}, Env: map[string]string{"X": "${A}:${B_KEY}", "Y": "plain"}}
	if got := strings.Join(e.References(), ","); got != "A,B_KEY" {
		t.Fatalf("references %s", got)
	}
	r := Entry{Name: "r", URL: "https://x.example.com/${TENANT}/mcp", Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"}}
	if got := strings.Join(r.References(), ","); got != "TENANT,TOKEN" {
		t.Fatalf("references %s", got)
	}
}

func TestAddFuncChecksFirst(t *testing.T) {
	called := false
	add := func(context.Context, Entry) (Added, error) { called = true; return Added{}, nil }
	if _, err := AddFunc(context.Background(), Entry{Name: "a"}, add); err == nil || called {
		t.Fatalf("err %v, host called %v", err, called)
	}
	if _, err := AddFunc(context.Background(), Entry{Name: "a", Command: []string{"x"}}, add); err != nil || !called {
		t.Fatalf("err %v, host called %v", err, called)
	}
}

func TestAddToolCall(t *testing.T) {
	var got Entry
	tool := AddTool(func(_ context.Context, e Entry) (Added, error) {
		got = e
		return Added{Tools: []string{"github__search", "github__issue"}, Missing: []string{"GITHUB_TOKEN"}, NextSession: true}, nil
	})
	if spec := tool.Spec(); spec.Name != "add_mcp_server" || !spec.Annotations.Destructive || !spec.Annotations.OpenWorld {
		t.Fatalf("spec %+v", spec)
	}
	in := `{"name":"github","command":["npx","-y","@org/server-github"],"env":[{"name":"GITHUB_TOKEN","value":"${GITHUB_TOKEN}"}],"source":"https://github.com/org/server-github"}`
	out, err := tool.Call(context.Background(), json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	want := Entry{Name: "github", Command: []string{"npx", "-y", "@org/server-github"}, Env: map[string]string{"GITHUB_TOKEN": "${GITHUB_TOKEN}"}, Source: "https://github.com/org/server-github"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("host got %+v", got)
	}
	if text := "Added MCP server github. It starts once the user sets: GITHUB_TOKEN. Its tools: github__search, github__issue. They are available from the next session: tell the user to start one."; out.Content != text {
		t.Fatalf("output %q", out.Content)
	}
}

func TestAddToolRefusalsReachTheModel(t *testing.T) {
	tool := AddTool(func(context.Context, Entry) (Added, error) { t.Fatal("host called"); return Added{}, nil })
	for in, want := range map[string]string{
		`{"name":"r","url":"https://x.example.com","headers":[{"name":"Authorization","value":"Bearer sk-1"}]}`: "never hold a key in clear",
		`{"name":"r","url":"http://x.example.com"}`:                                                             "https",
		`{"name":"a","command":["x"],"env":[{"name":"A","value":"1"},{"name":"A","value":"2"}]}`:                "twice",
	} {
		_, err := tool.Call(context.Background(), json.RawMessage(in))
		if msg := agent.ModelMessage(err); !strings.Contains(msg, want) {
			t.Errorf("%s: model sees %q, want %q", in, msg, want)
		}
	}
}

func TestAddToolHostErrorStaysPrivate(t *testing.T) {
	tool := AddTool(func(context.Context, Entry) (Added, error) {
		return Added{}, errors.New("HOST_IO", "cannot write /home/u/.config/app.yaml: token=abc")
	})
	_, err := tool.Call(context.Background(), json.RawMessage(`{"name":"a","command":["x"]}`))
	if msg := agent.ModelMessage(err); strings.Contains(msg, "token") || strings.Contains(msg, "/home") {
		t.Fatalf("model sees %q", msg)
	}
}

func TestAddToolAsksUnderDefaultPolicy(t *testing.T) {
	tool := AddTool(func(context.Context, Entry) (Added, error) { return Added{}, nil })
	call := agent.ToolCall{Name: "add_mcp_server", Input: json.RawMessage(`{"name":"a","command":["x"]}`)}
	if got := (&agent.Policy{}).Effect(call, tool.Spec()); got != agent.EffectAsk {
		t.Fatalf("effect %v, want ask", got)
	}
	if got := (&agent.Policy{Mode: agent.ModeAcceptEdits}).Effect(call, tool.Spec()); got != agent.EffectAsk {
		t.Fatalf("accept-edits effect %v, want ask", got)
	}
}
