// fake_test.go: a Messages API in memory, answering what a test scripts
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package anthropic

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
)

// fakeAPI answers each request with the next scripted message, as JSON or
// as the event stream the API sends, and keeps the requests it saw.
type fakeAPI struct {
	mu      sync.Mutex
	answers []map[string]any // the content and stop of each answer
	bodies  []map[string]any
	headers []http.Header
	status  int    // when set, every answer is this error status
	errBody string // with status
}

func newFake(t *testing.T, answers ...map[string]any) (*fakeAPI, string) {
	t.Helper()
	f := &fakeAPI{answers: answers}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(data, &body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.headers = append(f.headers, r.Header.Clone())
	var answer map[string]any
	if len(f.answers) > 0 {
		answer, f.answers = f.answers[0], f.answers[1:]
	}
	status, errBody := f.status, f.errBody
	f.mu.Unlock()
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, errBody)
		return
	}
	if answer == nil {
		answer = text("ok")
	}
	msg := message(answer)
	if stream, _ := body["stream"].(bool); stream {
		writeStream(w, msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}

// text is an answer of text that ends the turn.
func text(s string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "stop_reason": "end_turn"}
}

// thinkingThenCall is an answer that thinks, then calls tool with input.
func thinkingThenCall(signature, id, tool, input string) map[string]any {
	return map[string]any{"content": []any{
		map[string]any{"type": "thinking", "thinking": "", "signature": signature},
		map[string]any{"type": "tool_use", "id": id, "name": tool, "input": json.RawMessage(input)},
	}, "stop_reason": "tool_use"}
}

func message(answer map[string]any) map[string]any {
	return map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-test",
		"content": answer["content"], "stop_reason": answer["stop_reason"], "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 2}}
}

// writeStream sends msg as the API's events: start, each block (text in
// two deltas, tool input as JSON pieces), the stop, the end.
func writeStream(w http.ResponseWriter, msg map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	start := map[string]any{}
	for k, v := range msg {
		start[k] = v
	}
	start["content"] = []any{}
	start["stop_reason"] = nil
	event(w, "message_start", map[string]any{"type": "message_start", "message": start})
	for i, raw := range msg["content"].([]any) {
		streamBlock(w, i, raw.(map[string]any))
	}
	event(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": msg["stop_reason"], "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 5}})
	event(w, "message_stop", map[string]any{"type": "message_stop"})
}

func streamBlock(w http.ResponseWriter, i int, block map[string]any) {
	switch block["type"] {
	case "text":
		event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
		s := block["text"].(string)
		for _, piece := range []string{s[:len(s)/2], s[len(s)/2:]} {
			event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": piece}})
		}
	case "thinking":
		event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
		event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "signature_delta", "signature": block["signature"]}})
	case "tool_use":
		event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}})
		input := string(block["input"].(json.RawMessage))
		for _, piece := range []string{input[:len(input)/2], input[len(input)/2:]} {
			event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": piece}})
		}
	}
	event(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
}

func event(w http.ResponseWriter, name string, data any) {
	raw, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, raw)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// open is a provider talking to the fake at url.
func open(t *testing.T, url string, cfg Config) agent.Provider {
	t.Helper()
	cfg.BaseURL, cfg.APIKey = url, "sk-test-key"
	if cfg.Model == "" {
		cfg.Model = "claude-test"
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// sent is the n-th request body's messages, as JSON text, for matching.
func (f *fakeAPI) sent(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(f.bodies[n]["messages"])
	return string(raw)
}

func (f *fakeAPI) body(n int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[n]
}

func has(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
