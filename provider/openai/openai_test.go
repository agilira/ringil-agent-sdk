// openai_test.go: tests for configuration and request encoding
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

func mustNew(t *testing.T, cfg Config) *provider {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p.(*provider)
}

func TestEndpointURL(t *testing.T) {
	cases := []struct {
		base, want string
		allowHTTP  bool
	}{
		{"https://api.openai.com/v1", "https://api.openai.com/v1/chat/completions", false},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1/chat/completions", false},
		{"http://localhost:11434/v1", "http://localhost:11434/v1/chat/completions", false},
		{"http://127.0.0.1:8000", "http://127.0.0.1:8000/chat/completions", false},
		{"http://[::1]:8000/v1", "http://[::1]:8000/v1/chat/completions", false},
		{"http://LOCALHOST/v1", "http://LOCALHOST/v1/chat/completions", false},
		{"http://192.168.1.20:8000/v1", "http://192.168.1.20:8000/v1/chat/completions", true},
	}
	for _, c := range cases {
		got, err := endpointURL(c.base, c.allowHTTP)
		if err != nil || got != c.want {
			t.Errorf("endpointURL(%q) = %q, %v; want %q", c.base, got, err, c.want)
		}
	}
}

func TestEndpointURLRejects(t *testing.T) {
	for _, base := range []string{
		"http://192.168.1.20:8000/v1", // LAN without AllowHTTP
		"http://example.com/v1",
		"ftp://example.com",
		"https://user:pass@example.com/v1",
		"https://example.com/v1?key=x",
		"https://example.com/v1#frag",
		"https:///v1",
		"example.com/v1",
		"://bad",
		"",
	} {
		if _, err := endpointURL(base, false); !errors.HasCode(err, agent.ErrProviderConfig) {
			t.Errorf("endpointURL(%q) accepted or misclassified: %v", base, err)
		}
	}
}

func TestNewValidatesConfig(t *testing.T) {
	ok := Config{BaseURL: "https://x.test/v1", Model: "m"}
	bad := []func(*Config){
		func(c *Config) { c.Model = " " },
		func(c *Config) { c.MaxTokens = -1 },
		func(c *Config) { c.ContextWindow = -1 },
		func(c *Config) { c.APIKey = "key\r\nX-Injected: 1" },
		func(c *Config) { c.Headers = map[string]string{"authorization": "Bearer other"} },
		func(c *Config) { c.Headers = map[string]string{"Host": "evil.test"} },
		func(c *Config) { c.Headers = map[string]string{"Bad Name": "v"} },
		func(c *Config) { c.Headers = map[string]string{"": "v"} },
		func(c *Config) { c.Headers = map[string]string{"X-Ok": "line\nbreak"} },
		func(c *Config) { c.BaseURL = "http://example.com" },
	}
	for i, mutate := range bad {
		cfg := ok
		mutate(&cfg)
		if _, err := New(cfg); !errors.HasCode(err, agent.ErrProviderConfig) {
			t.Errorf("case %d accepted: %v", i, err)
		}
	}
	if _, err := New(ok); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
}

func TestNewCopiesHeadersAndClient(t *testing.T) {
	headers := map[string]string{"X-Title": "a"}
	client := &http.Client{Timeout: time.Second}
	p := mustNew(t, Config{BaseURL: "https://x.test", Model: "m", Headers: headers, Client: client})
	headers["X-Title"] = "changed"
	if p.cfg.Headers["X-Title"] != "a" {
		t.Fatal("provider shares the caller's header map")
	}
	if client.CheckRedirect != nil || p.client == client || p.client.Timeout != time.Second {
		t.Fatal("caller's client was modified or not copied")
	}
}

func TestInfo(t *testing.T) {
	p := mustNew(t, Config{BaseURL: "https://x.test", Model: "eos", ContextWindow: 32768})
	if info := p.Info(); info.Name != "eos" || info.ContextWindow != 32768 {
		t.Fatalf("Info = %+v", info)
	}
}

func TestStallTimeoutDefault(t *testing.T) {
	if stallTimeout(0) != DefaultStallTimeout || stallTimeout(-1) != -1 || stallTimeout(time.Second) != time.Second {
		t.Fatal("stallTimeout mapping is wrong")
	}
}

func TestAPIKeySources(t *testing.T) {
	p := mustNew(t, Config{BaseURL: "https://x.test", Model: "m", APIKey: "static"})
	if key, err := p.apiKey(context.Background()); err != nil || key != "static" {
		t.Fatalf("static key = %q", key)
	}
	p.cfg.KeyFunc = func(context.Context) (string, error) { return "dynamic", nil }
	if key, err := p.apiKey(context.Background()); err != nil || key != "dynamic" {
		t.Fatalf("KeyFunc key = %q", key)
	}
	p.cfg.KeyFunc = func(context.Context) (string, error) { return "", stderrors.New("vault down") }
	if _, err := p.apiKey(context.Background()); !errors.HasCode(err, agent.ErrProviderAuth) {
		t.Fatalf("KeyFunc error = %v", err)
	}
	p.cfg.KeyFunc = func(context.Context) (string, error) { return "a\nb", nil }
	if _, err := p.apiKey(context.Background()); !errors.HasCode(err, agent.ErrProviderAuth) {
		t.Fatalf("KeyFunc control chars = %v", err)
	}
}

func encodeFor(t *testing.T, cfg Config, req *agent.Request, stream bool) map[string]any {
	t.Helper()
	cfg.BaseURL, cfg.Model = "https://x.test", "m"
	p := mustNew(t, cfg)
	data, err := p.encode(req, stream)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEncodeConversation(t *testing.T) {
	temperature := 0.3
	req := &agent.Request{
		System: "sei utile",
		Messages: []agent.Message{
			agent.UserText("meteo a Roma?"),
			{Role: agent.RoleAssistant, Calls: []agent.ToolCall{{ID: "c1", Name: "meteo", Input: json.RawMessage(`{"citta":"Roma"}`)}}},
			agent.ToolMessage(agent.ToolResult{CallID: "c1", Content: "18 gradi"}),
			agent.AssistantText("18 gradi"),
		},
		Tools:       []agent.ToolSpec{{Name: "meteo", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens:   100,
		Temperature: &temperature,
		ToolChoice:  agent.ToolChoiceAuto,
	}
	got, err := json.Marshal(encodeFor(t, Config{}, req, false))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"max_tokens":100,"messages":[{"content":"sei utile","role":"system"},{"content":"meteo a Roma?","role":"user"},` +
		`{"content":null,"role":"assistant","tool_calls":[{"function":{"arguments":"{\"citta\":\"Roma\"}","name":"meteo"},"id":"c1","type":"function"}]},` +
		`{"content":"18 gradi","role":"tool","tool_call_id":"c1"},{"content":"18 gradi","role":"assistant"}],"model":"m","temperature":0.3,` +
		`"tool_choice":"auto","tools":[{"function":{"description":"d","name":"meteo","parameters":{"type":"object"}},"type":"function"}]}`
	if string(got) != want {
		t.Fatalf("encoding mismatch\n got %s\nwant %s", got, want)
	}
}

func TestEncodeDefaultsAndStream(t *testing.T) {
	req := &agent.Request{
		Messages: []agent.Message{
			{Role: agent.RoleAssistant, Calls: []agent.ToolCall{{ID: "c", Name: "t"}}},
			{Role: agent.RoleTool},
		},
		Tools: []agent.ToolSpec{{Name: "t"}},
	}
	out := encodeFor(t, Config{MaxTokens: 7}, req, true)
	if out["max_tokens"].(float64) != 7 || out["stream"] != true {
		t.Fatalf("defaults not applied: %v", out)
	}
	if opts := out["stream_options"].(map[string]any); opts["include_usage"] != true {
		t.Fatalf("stream usage not requested: %v", out)
	}
	msgs := out["messages"].([]any)
	call := msgs[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["function"].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("empty input not sent as {}: %v", call)
	}
	if msgs[1].(map[string]any)["content"] != "" {
		t.Fatalf("nil tool result not encoded as empty: %v", msgs[1])
	}
	params := out["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"]
	if params.(map[string]any)["type"] != "object" {
		t.Fatalf("empty schema not defaulted: %v", params)
	}
}

func TestEncodeOmitsUnset(t *testing.T) {
	out := encodeFor(t, Config{}, &agent.Request{Messages: []agent.Message{agent.UserText("x")}}, false)
	for _, key := range []string{"max_tokens", "temperature", "tool_choice", "tools", "stream", "stream_options"} {
		if _, ok := out[key]; ok {
			t.Errorf("%s sent although unset", key)
		}
	}
	if len(out["messages"].([]any)) != 1 {
		t.Fatal("empty system prompt was sent")
	}
}

func TestDecodeCall(t *testing.T) {
	cases := []struct {
		args, want string
		bad        bool
	}{
		{`{"a":1}`, `{"a":1}`, false},
		{``, `{}`, false},
		{`{"a":`, `{}`, true},
		{`not json`, `{}`, true},
	}
	for _, c := range cases {
		call := decodeCall("id", "t", c.args)
		if string(call.Input) != c.want || call.BadInput != c.bad || call.ID != "id" {
			t.Errorf("decodeCall(%q) = %+v", c.args, call)
		}
	}
	if call := decodeCall("", "t", "{}"); len(call.ID) < 10 || call.ID[:5] != "call_" {
		t.Fatalf("missing id not generated: %q", call.ID)
	}
	first, second := decodeCall("", "t", "{}"), decodeCall("", "t", "{}")
	if first.ID == second.ID {
		t.Fatal("generated ids collide")
	}
}

func TestDecodeStop(t *testing.T) {
	cases := map[string]agent.StopReason{
		"stop": agent.StopEnd, "": agent.StopEnd, "length": agent.StopMaxTokens,
		"content_filter": agent.StopFiltered, "tool_calls": agent.StopEnd,
	}
	for finish, want := range cases {
		if got := decodeStop(finish, 0); got != want {
			t.Errorf("decodeStop(%q) = %q, want %q", finish, got, want)
		}
	}
	if decodeStop("stop", 1) != agent.StopToolUse {
		t.Fatal("calls must win over a stop finish reason")
	}
}

func TestDecodeUsage(t *testing.T) {
	if (decodeUsage(nil) != agent.Usage{}) {
		t.Fatal("nil usage")
	}
	u := &wireUsage{PromptTokens: 10, CompletionTokens: -3}
	u.Details = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 4}
	if got := decodeUsage(u); got != (agent.Usage{Input: 10, Output: 0, CacheRead: 4}) {
		t.Fatalf("decodeUsage = %+v", got)
	}
}
