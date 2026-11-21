// security_test.go: what the environment, a server or a model could do
//
// THREAT MODEL:
//   - The environment: ANTHROPIC_* variables set for another program
//     (a base URL, a token, custom headers) must not redirect the prompts
//     or add a credential.
//   - A hostile or broken server, or one in the middle: redirects (the key
//     would follow), huge or endless bodies, a stream that stalls, answers
//     that are not what the API sends, error bodies that echo the prompt
//     or the key.
//   - A model's output: tool input that is not JSON, IDs missing.
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package anthropic

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// No ANTHROPIC_* variable decides where the request goes or what it
// carries: only Config does.
func TestSecurityEnvironmentIsNotRead(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(other.Close)
	t.Setenv("ANTHROPIC_BASE_URL", other.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-token")
	t.Setenv("ANTHROPIC_API_KEY", "env-key")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Leak: yes")
	f, url := newFake(t, text("ok"))
	if _, err := open(t, url, Config{}).Generate(t.Context(), ask("?"), nil); err != nil {
		t.Fatal(err)
	}
	h := f.headers[0]
	if elsewhere.Load() != 0 || h.Get("X-Leak") != "" || h.Get("Authorization") != "Bearer sk-test-key" || h.Get("X-Api-Key") != "" {
		t.Fatalf("the environment was read: elsewhere %d, headers %v", elsewhere.Load(), h)
	}
	if _, err := New(Config{Model: "m"}); !errors.HasCode(err, agent.ErrProviderConfig) {
		t.Fatalf("a key from the environment was taken: %v", err)
	}
}

// A redirect is never followed: the prompt and the key stay home.
func TestSecurityNoRedirect(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization") != "" || r.ContentLength > 0)
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	if _, err := open(t, srv.URL, Config{}).Generate(t.Context(), ask("segreto"), nil); err == nil {
		t.Fatal("a redirect was taken as the answer")
	}
	if leaked.Load() {
		t.Fatal("the request followed a redirect")
	}
}

// An error never carries what the server said: it may echo the prompt or
// the key.
func TestSecurityErrorsHoldNoUpstreamText(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 413, 429, 500, 529} {
		f, url := newFake(t)
		f.status = status
		f.errBody = `{"type":"error","error":{"type":"x","message":"ECHO sk-test-key the prompt"}}`
		for _, sink := range []agent.TextSink{nil, func(string) error { return nil }} {
			_, err := open(t, url, Config{}).Generate(t.Context(), ask("the prompt"), sink)
			if err == nil || strings.Contains(err.Error(), "ECHO") || strings.Contains(err.Error(), "sk-test-key") || strings.Contains(err.Error(), "prompt") {
				t.Errorf("%d: %v", status, err)
			}
		}
	}
}

// An endless body is cut at the bound, plain or streamed.
func TestSecurityHugeBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
			pad := "event: ping\ndata: {\"type\":\"ping\",\"pad\":\"" + strings.Repeat("x", 1<<20) + "\"}\n\n"
			for range maxBody/(1<<20) + 2 {
				if _, err := io.WriteString(w, pad); err != nil {
					return
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`)
		chunk := strings.Repeat("x", 1<<20)
		for range maxBody/(1<<20) + 2 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	for _, sink := range []agent.TextSink{nil, func(string) error { return nil }} {
		_, err := open(t, srv.URL, Config{}).Generate(t.Context(), ask("?"), sink)
		if !errors.HasCode(err, agent.ErrProviderResponse) {
			t.Errorf("stream %v: %v", sink != nil, err)
		}
	}
}

// A stream that goes silent is cut, as interrupted and retryable.
func TestSecurityStallIsCut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	start := time.Now()
	_, err := open(t, srv.URL, Config{StallTimeout: 200 * time.Millisecond}).Generate(t.Context(), ask("?"), func(string) error { return nil })
	if !errors.HasCode(err, agent.ErrProviderInterrupted) || !isRetryable(err) || time.Since(start) > 5*time.Second {
		t.Fatalf("after %v: %v", time.Since(start), err)
	}
}

// A server that takes the request and never answers is cut too.
func TestSecurityNoAnswerIsCut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // first: the handler ends, then the server
	start := time.Now()
	_, err := open(t, srv.URL, Config{StallTimeout: 200 * time.Millisecond}).Generate(t.Context(), ask("?"), nil)
	if !errors.HasCode(err, agent.ErrProviderInterrupted) || time.Since(start) > 5*time.Second {
		t.Fatalf("after %v: %v", time.Since(start), err)
	}
}

// A stream that ends before the message did is interrupted, not an
// empty answer.
func TestSecurityCutStreamIsInterrupted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		event(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "type": "message", "role": "assistant", "content": []any{}, "model": "x", "usage": map[string]any{}}})
		event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "mez"}})
	}))
	t.Cleanup(srv.Close)
	_, err := open(t, srv.URL, Config{}).Generate(t.Context(), ask("?"), func(string) error { return nil })
	if !errors.HasCode(err, agent.ErrProviderInterrupted) {
		t.Fatalf("%v", err)
	}
}

// Whatever a server answers, the provider fails cleanly or answers: no
// panic, an error with a provider code.
func FuzzServerAnswers(f *testing.F) {
	for _, s := range []string{`{}`, `null`, `[]`, `{"content":null}`, `{"content":[{"type":"tool_use","id":"","name":"x","input":"no"}]}`,
		`{"content":[{"type":"unknown_block"}],"stop_reason":"weird"}`, "\xff",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":5,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"o\"}}\n\n"} {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, body string, stream bool) {
		if len(body) > 8192 {
			return
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", map[bool]string{false: "application/json", true: "text/event-stream"}[stream])
			_, _ = io.WriteString(w, body)
		}))
		defer srv.Close()
		var sink agent.TextSink
		if stream {
			sink = func(string) error { return nil }
		}
		got, err := open(t, srv.URL, Config{}).Generate(t.Context(), ask("?"), sink)
		if err != nil {
			if !providerCode(err) {
				t.Fatalf("an error without a provider code: %v", err)
			}
			return
		}
		for _, c := range got.Message.Calls {
			if c.ID == "" || len(c.Input) == 0 {
				t.Fatalf("a call the agent cannot answer: %+v", c)
			}
		}
	})
}

func providerCode(err error) bool {
	for _, code := range []errors.ErrorCode{agent.ErrProviderConfig, agent.ErrProviderAuth, agent.ErrProviderRate, agent.ErrProviderUnavailable,
		agent.ErrProviderContext, agent.ErrProviderInput, agent.ErrProviderResponse, agent.ErrProviderInterrupted} {
		if errors.HasCode(err, code) {
			return true
		}
	}
	return false
}
