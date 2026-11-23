// chatgpt_test.go: requests, streamed answers, renewals, models
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// backend serves handler as the ChatGPT backend for a signed-in session.
func backend(t *testing.T, handler http.HandlerFunc) (agent.Provider, *Session, *issuer) {
	t.Helper()
	is := newIssuer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s, _ := signedIn(t, is)
	p, err := New(Config{Model: "gpt-x", Session: s, BaseURL: srv.URL, ReasoningEffort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	return p, s, is
}

func TestRequestShape(t *testing.T) {
	var body map[string]any
	var header http.Header
	p, _, _ := backend(t, func(w http.ResponseWriter, r *http.Request) {
		header = r.Header
		if r.URL.Path != "/responses" {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sse(w, completed)
	})
	img := agent.Part{Type: agent.PartImage, MIME: "image/png", Data: []byte("png")}
	req := &agent.Request{System: "be brief", Tools: []agent.ToolSpec{{Name: "read", Description: "reads", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []agent.Message{
			agent.UserMessage("look", img),
			{Role: agent.RoleAssistant, Parts: []agent.Part{
				{Type: agent.PartReasoning, Text: "mine", Signature: "enc-mine", Source: "chatgpt:gpt-x"},
				{Type: agent.PartReasoning, Text: "other", Signature: "enc-other", Source: "anthropic"},
				{Type: agent.PartText, Text: "calling"}},
				Calls: []agent.ToolCall{{ID: "c1", Name: "read", Input: json.RawMessage(`{"p":1}`)}}},
			agent.ToolMessage(agent.ToolResult{CallID: "c1", Content: ""}),
			agent.SystemText("host note"),
		}}
	if _, err := p.Generate(bg, req, nil); err != nil {
		t.Fatal(err)
	}
	checkHeaders(t, header)
	checkBody(t, body)
	checkInput(t, body["input"])
}

func checkHeaders(t *testing.T, header http.Header) {
	t.Helper()
	if header.Get("Authorization") == "" || header.Get("ChatGPT-Account-ID") != "acct-1" || header.Get("originator") != "ringil" {
		t.Fatalf("headers %v", header)
	}
}

func checkBody(t *testing.T, body map[string]any) {
	t.Helper()
	if body["store"] != false || body["stream"] != true || body["instructions"] != "be brief" || body["model"] != "gpt-x" {
		t.Fatalf("body %v", body)
	}
	if inc, _ := body["include"].([]any); len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Fatalf("include %v", body["include"])
	}
	if r, _ := body["reasoning"].(map[string]any); r["effort"] != "high" {
		t.Fatalf("reasoning %v", body["reasoning"])
	}
}

// checkInput: images, this model's reasoning only, the call and its
// empty output, the host's note as a developer message.
func checkInput(t *testing.T, input any) {
	t.Helper()
	data, _ := json.Marshal(input)
	in := string(data)
	for _, want := range []string{`"input_image"`, `data:image/png;base64,`, `"encrypted_content":"enc-mine"`, `"call_id":"c1","name":"read","type":"function_call"`, `"call_id":"c1","output":"","type":"function_call_output"`, `"role":"developer"`} {
		if !strings.Contains(in, want) {
			t.Errorf("input lacks %s:\n%s", want, in)
		}
	}
	if strings.Contains(in, "enc-other") {
		t.Errorf("another provider's reasoning was sent:\n%s", in)
	}
}

func TestStreamedAnswer(t *testing.T) {
	p, _, _ := backend(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w,
			`{"type":"response.output_text.delta","delta":"Hel"}`,
			`{"type":"response.output_text.delta","delta":"lo"}`,
			`{"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"ENC"}}`,
			`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"Hello"}]}}`,
			`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"c9","name":"read","arguments":"{\"a\":1}"}}`,
			`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"c10","name":"read","arguments":"{broken"}}`,
			completed)
	})
	var streamed strings.Builder
	resp, err := p.Generate(bg, &agent.Request{Messages: []agent.Message{agent.UserText("hi")}}, func(d string) error { streamed.WriteString(d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "Hello" || resp.Message.Text() != "Hello" || resp.Stop != agent.StopToolUse {
		t.Fatalf("%q %q %s", streamed.String(), resp.Message.Text(), resp.Stop)
	}
	checkAnswer(t, resp)
}

func checkAnswer(t *testing.T, resp *agent.Response) {
	t.Helper()
	if r := resp.Message.Reasoning("chatgpt:gpt-x"); len(r) != 1 || r[0].Signature != "ENC" || r[0].Text != "thought" {
		t.Fatalf("reasoning %+v", r)
	}
	if len(resp.Message.Calls) != 2 || string(resp.Message.Calls[0].Input) != `{"a":1}` || !resp.Message.Calls[1].BadInput {
		t.Fatalf("calls %+v", resp.Message.Calls)
	}
	if resp.Usage != (agent.Usage{Input: 10, Output: 3, CacheRead: 4}) {
		t.Fatalf("usage %+v", resp.Usage)
	}
}

func TestFailuresAreClassified(t *testing.T) {
	for event, code := range map[string]errors.ErrorCode{
		`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded"}}}`: agent.ErrProviderContext,
		`{"type":"response.failed","response":{"error":{"code":"usage_limit_reached"}}}`:     agent.ErrProviderRate,
		`{"type":"error","code":"rate_limit_exceeded"}`:                                      agent.ErrProviderRate,
		`{"type":"response.failed","response":{"error":{"code":"server_error"}}}`:            agent.ErrProviderUnavailable,
	} {
		p, _, _ := backend(t, func(w http.ResponseWriter, _ *http.Request) { sse(w, event) })
		if _, err := p.Generate(bg, &agent.Request{}, nil); !errors.HasCode(err, code) {
			t.Errorf("%s: %v", event, err)
		}
	}
	p, _, _ := backend(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`)
	})
	if resp, err := p.Generate(bg, &agent.Request{}, nil); err != nil || resp.Stop != agent.StopMaxTokens {
		t.Fatalf("incomplete: %v %v", resp, err)
	}
}

// A stream that ends before the response did is cut, retryable.
func TestCutStreamIsInterrupted(t *testing.T) {
	p, _, _ := backend(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"type":"response.output_text.delta","delta":"Hel"}`)
	})
	if _, err := p.Generate(bg, &agent.Request{}, nil); !errors.HasCode(err, agent.ErrProviderInterrupted) {
		t.Fatal(err)
	}
}

// A token refused once is renewed and the request sent again, once.
func TestRefusedTokenIsRenewedOnce(t *testing.T) {
	var calls atomic.Int32
	p, _, is := backend(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sse(w, completed)
	})
	if _, err := p.Generate(bg, &agent.Request{}, nil); err != nil || calls.Load() != 2 || is.refreshes.Load() != 1 {
		t.Fatalf("%v calls %d refreshes %d", err, calls.Load(), is.refreshes.Load())
	}
	p, _, _ = backend(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	if _, err := p.Generate(bg, &agent.Request{}, nil); !errors.HasCode(err, agent.ErrProviderAuth) {
		t.Fatalf("refused twice: %v", err)
	}
}

func TestSignedOutIsACredentialError(t *testing.T) {
	p, err := New(Config{Model: "m", Session: &Session{Store: &memStore{}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Generate(bg, &agent.Request{}, nil)
	if !errors.HasCode(err, agent.ErrProviderAuth) || !strings.Contains(err.Error(), "sign in") {
		t.Fatal(err)
	}
}

func TestConfigChecked(t *testing.T) {
	s := &Session{Store: &memStore{}}
	for _, cfg := range []Config{
		{Session: s},
		{Model: "m"},
		{Model: "m", Session: s, ReasoningEffort: "max"},
		{Model: "m", Session: s, BaseURL: "http://example.com"},
		{Model: "m", Session: s, BaseURL: "https://u:p@example.com"},
	} {
		if _, err := New(cfg); !errors.HasCode(err, agent.ErrProviderConfig) {
			t.Errorf("%+v: %v", cfg, err)
		}
	}
}

func TestModels(t *testing.T) {
	is := newIssuer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.URL.Query().Get("client_version") == "" || r.Header.Get("ChatGPT-Account-ID") != "acct-1" {
			t.Errorf("request %s %v", r.URL, r.Header)
		}
		_, _ = io.WriteString(w, `{"models":[
			{"slug":"second","display_name":"Second","visibility":"list","priority":3,"context_window":272000},
			{"slug":"hidden","visibility":"hide","priority":1},
			{"slug":"first","display_name":"First","visibility":"list","priority":2,"default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`)
	}))
	defer srv.Close()
	s, _ := signedIn(t, is)
	models, err := Models(bg, s, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{{Name: "first", Title: "First", Effort: "medium", Efforts: []string{"low", "high"}}, {Name: "second", Title: "Second", ContextWindow: 272000}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("%+v", models)
	}
}

func TestTokensClaims(t *testing.T) {
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	tok := access("acct-9", exp)
	if accountOf("garbage", tok) != "acct-9" || !expiryOf(tok).Equal(exp) || !expiryOf("x.y.z").IsZero() {
		t.Fatal("claims")
	}
}

// Structured output goes as text.format in the Responses API.
func TestTextFormat(t *testing.T) {
	type answer struct {
		A string `json:"a"`
	}
	format, err := agent.FormatFor[answer]("answer")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	p, _, _ := backend(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		sse(w, completed)
	})
	if _, err := p.Generate(bg, &agent.Request{Messages: []agent.Message{agent.UserText("x")}, Format: format}, nil); err != nil {
		t.Fatal(err)
	}
	text, _ := body["text"].(map[string]any)
	f, _ := text["format"].(map[string]any)
	if f["type"] != "json_schema" || f["name"] != "answer" || f["strict"] != true || f["schema"] == nil {
		t.Fatalf("%v", body["text"])
	}
}
