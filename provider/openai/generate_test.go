// generate_test.go: tests for calls against a real HTTP server
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
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

func serve(t *testing.T, handler http.HandlerFunc) *provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return mustNew(t, Config{BaseURL: srv.URL + "/v1", Model: "m", APIKey: "secret"})
}

// reply writes a canned body from a server handler.
func reply(t *testing.T, w io.Writer, body string) {
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("handler write: %v", err)
	}
}

func ask(p *provider, sink agent.TextSink) (*agent.Response, error) {
	return p.Generate(context.Background(), &agent.Request{Messages: []agent.Message{agent.UserText("ciao")}}, sink)
}

func TestGenerateSendsHeadersAndPath(t *testing.T) {
	var seen *http.Request
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		reply(t, w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	})
	p.cfg.Headers = map[string]string{"X-Title": "harness"}
	resp, err := ask(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkSentRequest(t, seen)
	if resp.Message.Text() != "ok" || resp.Stop != agent.StopEnd || resp.Usage.Input != 3 {
		t.Fatalf("response %+v", resp)
	}
}

func checkSentRequest(t *testing.T, seen *http.Request) {
	t.Helper()
	if seen.URL.Path != "/v1/chat/completions" || seen.Method != http.MethodPost {
		t.Fatalf("request %s %s", seen.Method, seen.URL.Path)
	}
	if seen.Header.Get("Authorization") != "Bearer secret" || seen.Header.Get("X-Title") != "harness" ||
		seen.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", seen.Header)
	}
}

func TestGenerateWithoutKeySendsNoAuthorization(t *testing.T) {
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply(t, w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	p.cfg.APIKey = ""
	if _, err := ask(p, nil); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateToolCalls(t *testing.T) {
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		reply(t, w, `{"choices":[{"message":{"content":null,"tool_calls":[`+
			`{"id":"a","type":"function","function":{"name":"meteo","arguments":"{\"citta\":\"Roma\"}"}},`+
			`{"type":"function","function":{"name":"rotto","arguments":"{\"x\":"}}]},"finish_reason":"tool_calls"}]}`)
	})
	resp, err := ask(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.Calls) != 2 || resp.Stop != agent.StopToolUse || resp.Message.Parts != nil {
		t.Fatalf("response %+v", resp)
	}
	checkCalls(t, resp.Message.Calls)
}

func checkCalls(t *testing.T, calls []agent.ToolCall) {
	t.Helper()
	if calls[0].ID != "a" || string(calls[0].Input) != `{"citta":"Roma"}` || calls[0].BadInput {
		t.Fatalf("first call %+v", calls[0])
	}
	if calls[1].ID == "" || !calls[1].BadInput || string(calls[1].Input) != "{}" {
		t.Fatalf("broken call %+v", calls[1])
	}
}

func TestGenerateMalformedBodies(t *testing.T) {
	for _, body := range []string{`not json`, `{"choices":[]}`, `{}`, `{"error":{"message":"x"}}`} {
		p := serve(t, func(w http.ResponseWriter, r *http.Request) { reply(t, w, body) })
		if _, err := ask(p, nil); !errors.HasCode(err, agent.ErrProviderResponse) {
			t.Errorf("body %q: %v", body, err)
		}
	}
}

func TestGenerateBodyBound(t *testing.T) {
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		reply(t, w, `{"choices":[{"message":{"content":"`)
		chunk := strings.Repeat("a", 1<<20)
		for range maxBody>>20 + 1 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return // the client stopped reading at the bound, which is the point
			}
		}
	})
	if _, err := ask(p, nil); !errors.HasCode(err, agent.ErrProviderResponse) {
		t.Fatalf("oversized body: %v", err)
	}
}

func TestGenerateStatusClassification(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		code      errors.ErrorCode
		retryable bool
	}{
		{401, `{}`, agent.ErrProviderAuth, false},
		{403, `{}`, agent.ErrProviderAuth, false},
		{429, `{"error":{"code":"rate_limit_exceeded"}}`, agent.ErrProviderRate, true},
		{429, `{"error":{"code":"insufficient_quota"}}`, agent.ErrProviderRate, false},
		{400, `{"error":{"code":"context_length_exceeded"}}`, agent.ErrProviderContext, false},
		{400, `{"error":"This model's Maximum Context Length is 8192 tokens"}`, agent.ErrProviderContext, false},
		{413, `{}`, agent.ErrProviderContext, false},
		{400, `{"error":"bad schema"}`, agent.ErrProviderInput, false},
		{422, `{}`, agent.ErrProviderInput, false},
		{404, `{}`, agent.ErrProviderConfig, false},
		{408, `{}`, agent.ErrProviderUnavailable, true},
		{500, `{}`, agent.ErrProviderUnavailable, true},
		{503, `{}`, agent.ErrProviderUnavailable, true},
		{529, `{}`, agent.ErrProviderUnavailable, true},
		{501, `{}`, agent.ErrProviderInput, false},
	}
	for _, c := range cases {
		p := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			reply(t, w, c.body)
		})
		_, err := ask(p, nil)
		if !errors.HasCode(err, c.code) || isRetryable(err) != c.retryable {
			t.Errorf("status %d %s: %v (retryable %v)", c.status, c.body, err, isRetryable(err))
		}
	}
}

func isRetryable(err error) bool {
	var r errors.Retryable
	return stderrors.As(err, &r) && r.IsRetryable()
}

func TestErrorsNeverCarryUpstreamBody(t *testing.T) {
	const marker = "PROMPT-ECHO-sk-live-123"
	for _, status := range []int{400, 401, 404, 429, 500} {
		p := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			reply(t, w, `{"error":{"message":"`+marker+`"}}`)
		})
		_, err := ask(p, nil)
		if err == nil || strings.Contains(fmt.Sprintf("%+v %#v", err, err), marker) {
			t.Errorf("status %d leaked the body: %v", status, err)
		}
	}
}

func TestRetryAfterHeader(t *testing.T) {
	for _, value := range []string{"7", time.Now().Add(7 * time.Second).UTC().Format(http.TimeFormat)} {
		p := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", value)
			w.WriteHeader(http.StatusTooManyRequests)
		})
		_, err := ask(p, nil)
		wait, ok := agent.RetryAfter(err)
		if !ok || wait < 5*time.Second || wait > 7*time.Second {
			t.Errorf("Retry-After %q gave %v %v", value, wait, ok)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var leaked atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer evil.Close()
	for _, status := range []int{301, 302, 307, 308} {
		p := serve(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, evil.URL+"/steal", status)
		})
		if _, err := ask(p, nil); !errors.HasCode(err, agent.ErrProviderInput) {
			t.Errorf("redirect %d: %v", status, err)
		}
	}
	if leaked.Load() {
		t.Fatal("a redirect was followed")
	}
}

func TestNetworkFailureIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	p := mustNew(t, Config{BaseURL: url, Model: "m"})
	_, err := ask(p, nil)
	if !errors.HasCode(err, agent.ErrProviderUnavailable) || !isRetryable(err) {
		t.Fatalf("closed server: %v", err)
	}
}

func TestCancellationStaysContextError(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.Generate(ctx, &agent.Request{}, nil)
	if !stderrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}

func TestStalledBodyIsInterrupted(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		reply(t, w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	p.stall = 50 * time.Millisecond
	start := time.Now()
	_, err := ask(p, func(string) error { return nil })
	if !errors.HasCode(err, agent.ErrProviderInterrupted) || !isRetryable(err) || time.Since(start) > 5*time.Second {
		t.Fatalf("stall: %v after %v", err, time.Since(start))
	}
}

// A server that takes the request and never answers is cut too (audit
// 21/11: the stall began only with the body).
func TestNoAnswerIsInterrupted(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	p := serve(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	p.stall = 50 * time.Millisecond
	start := time.Now()
	for _, sink := range []agent.TextSink{nil, func(string) error { return nil }} {
		_, err := ask(p, sink)
		if !errors.HasCode(err, agent.ErrProviderInterrupted) || !isRetryable(err) || time.Since(start) > 5*time.Second {
			t.Fatalf("no answer: %v after %v", err, time.Since(start))
		}
	}
}

func TestNilRequest(t *testing.T) {
	p := mustNew(t, Config{BaseURL: "https://x.test", Model: "m"})
	if _, err := p.Generate(context.Background(), nil, nil); !errors.HasCode(err, agent.ErrProviderInput) {
		t.Fatalf("nil request: %v", err)
	}
}

func TestKeyFuncErrorStopsBeforeSending(t *testing.T) {
	var hits atomic.Int32
	p := serve(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	p.cfg.KeyFunc = func(context.Context) (string, error) { return "", stderrors.New("locked") }
	if _, err := ask(p, nil); !errors.HasCode(err, agent.ErrProviderAuth) || hits.Load() != 0 {
		t.Fatalf("err %v hits %d", err, hits.Load())
	}
}

func TestUnencodableRequest(t *testing.T) {
	p := mustNew(t, Config{BaseURL: "https://x.test", Model: "m"})
	req := &agent.Request{Tools: []agent.ToolSpec{{Name: "t", InputSchema: []byte(`{bad`)}}}
	if _, err := p.Generate(context.Background(), req, nil); !errors.HasCode(err, agent.ErrProviderInput) {
		t.Fatalf("bad schema: %v", err)
	}
}

func TestBodyReadFailureIsInterrupted(t *testing.T) {
	if _, err := decodeBody(io.MultiReader(strings.NewReader(`{"choi`), errReader{})); !errors.HasCode(err, agent.ErrProviderInterrupted) {
		t.Fatalf("read failure: %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, stderrors.New("connection reset") }

// brokenBody serves content, then fails on Close like a broken connection.
type brokenBody struct {
	io.Reader
	closeErr error
}

func (b brokenBody) Close() error { return b.closeErr }

func TestCloseFailureMakesCallInterrupted(t *testing.T) {
	p := mustNew(t, Config{BaseURL: "https://x.test", Model: "m"})
	body := `{"choices":[{"message":{"content":"ok"}}]}`
	resp := &http.Response{Body: brokenBody{strings.NewReader(body), stderrors.New("reset")}}
	_, err := p.read(resp, nil, func(error) {})
	if !errors.HasCode(err, agent.ErrProviderInterrupted) || !isRetryable(err) {
		t.Fatalf("close failure: %v", err)
	}
	resp = &http.Response{Body: brokenBody{strings.NewReader(`garbage`), stderrors.New("reset")}}
	if _, err := p.read(resp, nil, func(error) {}); !errors.HasCode(err, agent.ErrProviderResponse) {
		t.Fatalf("decode failure lost its code behind the close failure: %v", err)
	}
}

// A model's reasoning (reasoning_content) is kept, and goes back to that
// model within the round of tool calls only (DeepSeek's thinking mode
// wants it there; servers refuse it after).
func TestReasoningGoesBackWithinTheRound(t *testing.T) {
	var bodies []string
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(data))
		reply(t, w, `{"choices":[{"message":{"content":"","reasoning_content":"think A","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	})
	req := &agent.Request{Messages: []agent.Message{agent.UserText("go")}}
	first, err := p.Generate(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := first.Message.Reasoning("openai:m"); len(r) != 1 || r[0].Text != "think A" || r[0].Source != "openai:m" || first.Message.Text() != "" {
		t.Fatalf("%+v", first.Message.Parts)
	}
	req.Messages = append(req.Messages, first.Message, agent.ToolMessage(agent.ToolResult{CallID: "c1", Content: "ok"}))
	if _, err := p.Generate(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies[1], `"reasoning_content":"think A"`) {
		t.Fatalf("in the round: %s", bodies[1])
	}
	req.Messages = append(req.Messages, agent.AssistantText("done"), agent.UserText("next"))
	notAfterTheRound(t, p, req, &bodies)
}

// notAfterTheRound: once the user writes again, the reasoning goes to no
// model, this one or another.
func notAfterTheRound(t *testing.T, p *provider, req *agent.Request, bodies *[]string) {
	t.Helper()
	other := mustNew(t, Config{BaseURL: p.cfg.BaseURL, Model: "other", APIKey: "secret"})
	other.endpoint, other.client = p.endpoint, p.client
	for _, q := range []*provider{p, other} {
		if _, err := q.Generate(context.Background(), req, nil); err != nil {
			t.Fatal(err)
		}
		if last := (*bodies)[len(*bodies)-1]; strings.Contains(last, "reasoning_content") {
			t.Fatalf("after the round, or to another model: %s", last)
		}
	}
}

// A tool's images go to an OpenAI-compatible model in a user message
// after the results (tool messages carry text only); to one without
// images, a note.
func TestToolResultImages(t *testing.T) {
	var bodies []string
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	for _, images := range []bool{true, false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(data))
			reply(t, w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
		}))
		p := mustNew(t, Config{BaseURL: srv.URL + "/v1", Model: "m", Images: images})
		calls := agent.Message{Role: agent.RoleAssistant, Calls: []agent.ToolCall{{ID: "c1", Name: "shot", Input: json.RawMessage(`{}`)}}}
		res := agent.ToolResult{CallID: "c1", Content: "screen", Images: []agent.Part{{Type: agent.PartImage, MIME: "image/png", Data: png}}}
		if _, err := p.Generate(context.Background(), &agent.Request{Messages: []agent.Message{agent.UserText("guarda"), calls, agent.ToolMessage(res)}}, nil); err != nil {
			t.Fatal(err)
		}
		srv.Close()
	}
	if !strings.Contains(bodies[0], `"tool_call_id":"c1"`) || !strings.Contains(bodies[0], `data:image/png;base64,`) {
		t.Fatalf("with images: %s", bodies[0])
	}
	if strings.Contains(bodies[1], "data:image") || !strings.Contains(bodies[1], "cannot see") {
		t.Fatalf("without: %s", bodies[1])
	}
}

// The request takes what other servers need: max_completion_tokens
// (OpenAI's reasoning models; by default at api.openai.com),
// reasoning_effort and the host's own fields, which never replace the
// provider's.
func TestRequestOptions(t *testing.T) {
	var body map[string]any
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		reply(t, w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	})
	p.cfg.MaxCompletionTokens, p.cfg.ReasoningEffort, p.cfg.MaxTokens = true, "low", 500
	p.cfg.Extra = map[string]any{"top_k": 20, "model": "other", "provider": map[string]any{"sort": "price"}}
	if _, err := ask(p, nil); err != nil {
		t.Fatal(err)
	}
	if body["max_completion_tokens"] != float64(500) || body["max_tokens"] != nil || body["reasoning_effort"] != "low" ||
		body["top_k"] != float64(20) || body["model"] != "m" || body["provider"] == nil {
		t.Fatalf("%v", body)
	}
	if oai := mustNew(t, Config{BaseURL: "https://api.openai.com/v1", Model: "gpt-5", APIKey: "k"}); !oai.cfg.MaxCompletionTokens {
		t.Fatal("api.openai.com sends max_tokens")
	}
}
