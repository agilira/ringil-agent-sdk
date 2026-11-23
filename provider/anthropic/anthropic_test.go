// anthropic_test.go: the provider against a fake Messages API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package anthropic

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

var lookup = agent.ToolSpec{Name: "lookup", Description: "Look a word up",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"word":{"type":"string"}},"required":["word"],"additionalProperties":false,"$defs":{"x":{"type":"string"}}}`)}

func ask(text string) *agent.Request {
	return &agent.Request{System: "Be brief.", Messages: []agent.Message{agent.UserText(text)}}
}

func TestPlainAnswer(t *testing.T) {
	f, url := newFake(t, text("Parigi"))
	p := open(t, url, Config{})
	got, err := p.Generate(t.Context(), ask("Capitale della Francia?"), nil)
	if err != nil || got.Message.Text() != "Parigi" || got.Stop != agent.StopEnd {
		t.Fatalf("%+v %v", got, err)
	}
	if got.Usage != (agent.Usage{Input: 15, Output: 5, CacheRead: 3, CacheWrite: 2}) { // the prompt whole
		t.Fatalf("usage %+v", got.Usage)
	}
	body := f.body(0)
	if body["model"] != "claude-test" || body["max_tokens"] != float64(defaultMaxTokens) {
		t.Fatalf("body %v", body)
	}
	if sys, _ := json.Marshal(body["system"]); !has(string(sys), "Be brief.") {
		t.Fatalf("system %s", sys)
	}
}

func TestStreamedAnswer(t *testing.T) {
	f, url := newFake(t, text("Parigi, in Francia"))
	p := open(t, url, Config{})
	var deltas []string
	got, err := p.Generate(t.Context(), ask("Capitale?"), func(d string) error { deltas = append(deltas, d); return nil })
	if err != nil || got.Message.Text() != "Parigi, in Francia" || strings.Join(deltas, "") != got.Message.Text() || len(deltas) != 2 {
		t.Fatalf("%+v %q %v", got, deltas, err)
	}
	if f.body(0)["max_tokens"] != float64(defaultMaxTokens) || f.body(0)["stream"] != true {
		t.Fatalf("max tokens %v", f.body(0)["max_tokens"])
	}
}

// A streamed call: its input arrives in pieces and comes back whole; the
// thinking before it is not text.
func TestStreamedCall(t *testing.T) {
	_, url := newFake(t, thinkingThenCall("sig-1", "toolu_1", "lookup", `{"word":"ciao"}`))
	p := open(t, url, Config{})
	var streamed strings.Builder
	got, err := p.Generate(t.Context(), ask("?"), func(d string) error { streamed.WriteString(d); return nil })
	if err != nil || got.Stop != agent.StopToolUse || len(got.Message.Calls) != 1 || streamed.Len() != 0 {
		t.Fatalf("%+v %q %v", got, streamed.String(), err)
	}
	if c := got.Message.Calls[0]; c.ID != "toolu_1" || c.Name != "lookup" || string(c.Input) != `{"word":"ciao"}` || c.BadInput {
		t.Fatalf("call %+v", c)
	}
}

// The thinking of a round goes back with its turn, before its calls, while
// the round lasts; after the user writes again it is left out.
// The thinking goes back at every turn, as the API keeps it (21/11: it
// was left out after each round, when the system prompt still changed
// between turns); a saved conversation keeps it in its reasoning parts.
func TestThinkingGoesBackAtEveryTurn(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		var sink agent.TextSink
		if streamed {
			sink = func(string) error { return nil }
		}
		thinkingRound(t, sink)
	}
}

func thinkingRound(t *testing.T, sink agent.TextSink) {
	t.Helper()
	streamed := sink != nil
	f, url := newFake(t, thinkingThenCall("sig-round", "toolu_1", "lookup", `{"word":"a"}`), text("fatto"), text("ancora"), text("di nuovo"))
	p := open(t, url, Config{})
	req := &agent.Request{Messages: []agent.Message{agent.UserText("cerca a")}, Tools: []agent.ToolSpec{lookup}}
	first, err := p.Generate(t.Context(), req, sink)
	if err != nil {
		t.Fatal(err)
	}
	if r := first.Message.Reasoning("anthropic"); len(r) != 1 || r[0].Signature != "sig-round" || first.Message.Text() != "" {
		t.Fatalf("reasoning %+v", first.Message.Parts)
	}
	req.Messages = append(req.Messages, first.Message, agent.ToolMessage(agent.ToolResult{CallID: "toolu_1", Content: "trovato"}))
	second, err := p.Generate(t.Context(), req, sink)
	if err != nil {
		t.Fatal(err)
	}
	if sent := f.sent(1); !has(sent, `"signature":"sig-round"`, `"type":"tool_result"`) || strings.Index(sent, "sig-round") > strings.Index(sent, `"tool_use"`) {
		t.Fatalf("streamed %v, the round went back as %s", streamed, sent)
	}
	req.Messages = append(req.Messages, second.Message, agent.UserText("e poi?"))
	afterTheRound(t, f, url, p, req, sink)
}

// afterTheRound: the thinking still goes back after the user writes
// again, and from a provider resumed elsewhere (a new process) too.
func afterTheRound(t *testing.T, f *fakeAPI, url string, p agent.Provider, req *agent.Request, sink agent.TextSink) {
	t.Helper()
	if _, err := p.Generate(t.Context(), req, sink); err != nil {
		t.Fatal(err)
	}
	if sent := f.sent(2); !has(sent, `"signature":"sig-round"`, `"tool_use"`, `"tool_result"`) {
		t.Fatalf("after the round: %s", sent)
	}
	if _, err := open(t, url, Config{}).Generate(t.Context(), req, sink); err != nil || !strings.Contains(f.sent(3), `"signature":"sig-round"`) {
		t.Fatalf("resumed: %v %s", err, f.sent(3))
	}
}

// The turn goes back as the model made it: blocks in their order, thinking
// between them where it was (audit 21/11: they were regrouped).
func TestTheTurnGoesBackAsMade(t *testing.T) {
	interleaved := map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "Cerco."},
		map[string]any{"type": "thinking", "thinking": "", "signature": "S1"},
		map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": json.RawMessage(`{"word":"a"}`)},
		map[string]any{"type": "thinking", "thinking": "", "signature": "S2"},
		map[string]any{"type": "tool_use", "id": "toolu_2", "name": "lookup", "input": json.RawMessage(`{"word":"b"}`)},
	}, "stop_reason": "tool_use"}
	f, url := newFake(t, interleaved, text("ok"))
	p := open(t, url, Config{})
	req := &agent.Request{Messages: []agent.Message{agent.UserText("cerca")}, Tools: []agent.ToolSpec{lookup}}
	first, err := p.Generate(t.Context(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, first.Message,
		agent.ToolMessage(agent.ToolResult{CallID: "toolu_1", Content: "A"}), agent.ToolMessage(agent.ToolResult{CallID: "toolu_2", Content: "B"}))
	if _, err := p.Generate(t.Context(), req, nil); err != nil {
		t.Fatal(err)
	}
	sent := f.sent(1)
	order := []string{`"Cerco."`, `"S1"`, `"toolu_1"`, `"S2"`, `"toolu_2"`}
	for i := 1; i < len(order); i++ {
		if strings.Index(sent, order[i-1]) > strings.Index(sent, order[i]) || !strings.Contains(sent, order[i]) {
			t.Fatalf("order %v in %s", order, sent)
		}
	}
}

// With NoThinking (no block_binding) the round's thinking does not go
// back: a round the agent changed would be refused, not dropped.
func TestNoThinkingSendsNoneBack(t *testing.T) {
	f, url := newFake(t, thinkingThenCall("sig-x", "toolu_1", "lookup", `{"word":"a"}`), text("ok"))
	p := open(t, url, Config{NoThinking: true})
	req := &agent.Request{Messages: []agent.Message{agent.UserText("cerca")}, Tools: []agent.ToolSpec{lookup}}
	first, err := p.Generate(t.Context(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, first.Message, agent.ToolMessage(agent.ToolResult{CallID: "toolu_1", Content: "A"}))
	if _, err := p.Generate(t.Context(), req, nil); err != nil || strings.Contains(f.sent(1), "sig-x") {
		t.Fatalf("%v %s", err, f.sent(1))
	}
}

// A large output limit works without a sink too (audit 21/11: the SDK
// refused a plain request over ~21000 tokens, as a retryable failure).
func TestLargeLimitWithoutSink(t *testing.T) {
	_, url := newFake(t, text("ok"))
	if got, err := open(t, url, Config{MaxTokens: 128000}).Generate(t.Context(), ask("?"), nil); err != nil || got.Message.Text() != "ok" {
		t.Fatalf("%+v %v", got, err)
	}
}

// An error event in a stream that began well is the server's: retryable,
// whatever its type (audit 21/11: an api_error came back as the input's).
func TestErrorMidStreamIsRetryable(t *testing.T) {
	for _, kind := range []string{"api_error", "overloaded_error"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			event(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "type": "message", "role": "assistant", "content": []any{}, "model": "x", "usage": map[string]any{}}})
			event(w, "ping", map[string]any{"type": "ping"})
			event(w, "error", map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": "Internal server error"}})
		}))
		_, err := open(t, srv.URL, Config{}).Generate(t.Context(), ask("?"), func(string) error { return nil })
		srv.Close()
		if !errors.HasCode(err, agent.ErrProviderUnavailable) || !isRetryable(err) {
			t.Errorf("%s: %v", kind, err)
		}
	}
}

// A model that refuses a forced tool choice gets "required" in words: the
// call goes on, and the next ones ask in words from the start.
func TestRequiredOnAModelThatRefusesForcing(t *testing.T) {
	var forced int
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(data))
		if strings.Contains(string(data), `"tool_choice":{"type":"any"}`) {
			forced++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tool_choice: type \"tool\" and \"any\" are not supported for this model."}}`)
			return
		}
		writeStream(w, message(thinkingThenCall("s", "toolu_1", "lookup", `{"word":"a"}`)))
	}))
	t.Cleanup(srv.Close)
	p := open(t, srv.URL, Config{})
	req := &agent.Request{Messages: []agent.Message{agent.UserText("cerca")}, Tools: []agent.ToolSpec{lookup}, ToolChoice: agent.ToolChoiceRequired}
	for range 2 {
		got, err := p.Generate(t.Context(), req, nil)
		if err != nil || got.Stop != agent.StopToolUse {
			t.Fatalf("%+v %v", got, err)
		}
	}
	last := bodies[len(bodies)-1]
	if forced != 1 || len(bodies) != 3 || !has(last, `"tool_choice":{"type":"auto"}`, `"role":"system"`, "must call one of the tools") {
		t.Fatalf("forced %d, %d requests, last %s", forced, len(bodies), last)
	}
}

// The results of a round are one user turn, and roles alternate.
func TestResultsOfARoundAreOneTurn(t *testing.T) {
	f, url := newFake(t, text("ok"))
	p := open(t, url, Config{})
	calls := agent.Message{Role: agent.RoleAssistant, Calls: []agent.ToolCall{
		{ID: "toolu_a", Name: "lookup", Input: json.RawMessage(`{"word":"a"}`)},
		{ID: "toolu_b", Name: "lookup", Input: json.RawMessage(`{"word":"b"}`)},
	}}
	req := &agent.Request{Messages: []agent.Message{agent.UserText("cerca"), calls,
		agent.ToolMessage(agent.ToolResult{CallID: "toolu_a", Content: "A"}),
		agent.ToolMessage(agent.ToolResult{CallID: "toolu_b", Content: "B", IsError: true})}}
	if _, err := p.Generate(t.Context(), req, nil); err != nil {
		t.Fatal(err)
	}
	var msgs []struct {
		Role    string           `json:"role"`
		Content []map[string]any `json:"content"`
	}
	_ = json.Unmarshal([]byte(f.sent(0)), &msgs)
	if len(msgs) != 3 || msgs[0].Role != "user" || msgs[1].Role != "assistant" || msgs[2].Role != "user" || len(msgs[2].Content) != 2 {
		t.Fatalf("turns %s", f.sent(0))
	}
	if msgs[2].Content[1]["is_error"] != true || msgs[2].Content[0]["tool_use_id"] != "toolu_a" {
		t.Fatalf("results %v", msgs[2].Content)
	}
}

// A tool's schema goes as it is, whatever else it says.
func TestSchemaAsItIs(t *testing.T) {
	f, url := newFake(t, text("ok"))
	p := open(t, url, Config{})
	req := ask("?")
	req.Tools = []agent.ToolSpec{lookup, {Name: "none"}}
	if _, err := p.Generate(t.Context(), req, nil); err != nil {
		t.Fatal(err)
	}
	tools, _ := json.Marshal(f.body(0)["tools"])
	if !has(string(tools), `"required":["word"]`, `"additionalProperties":false`, `"$defs"`, `"word":{"type":"string"}`, `"name":"none"`, `"type":"object"`) {
		t.Fatalf("tools %s", tools)
	}
	req.Tools = []agent.ToolSpec{{Name: "bad", InputSchema: json.RawMessage(`[1]`)}}
	if _, err := p.Generate(t.Context(), req, nil); !errors.HasCode(err, agent.ErrProviderInput) {
		t.Fatalf("a schema that is not an object: %v", err)
	}
}

func TestRequestSettings(t *testing.T) {
	f, url := newFake(t, text("a"), text("b"), text("c"))
	temp := 0.2
	req := ask("?")
	req.ToolChoice, req.Temperature, req.MaxTokens = agent.ToolChoiceRequired, &temp, 123
	if _, err := open(t, url, Config{Effort: "low", MaxTokens: 999}).Generate(t.Context(), req, nil); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(f.body(0))
	if !has(string(body), `"tool_choice":{"type":"any"}`, `"max_tokens":123`, `"effort":"low"`, `"prefix_mismatch_behavior":"drop_block"`, `"type":"adaptive"`, `"cache_control":{"type":"ephemeral"}`) ||
		strings.Contains(string(body), "temperature") {
		t.Fatalf("body %s", body)
	}
	if beta := f.headers[0].Get("Anthropic-Beta"); !strings.Contains(beta, "thinking-binding-controls-2026-08-01") || f.headers[0].Get("Authorization") != "Bearer sk-test-key" {
		t.Fatalf("headers %v", f.headers[0])
	}
}

func TestNoThinking(t *testing.T) {
	f, url := newFake(t, text("a"))
	if _, err := open(t, url, Config{NoThinking: true, MaxTokens: 999}).Generate(t.Context(), ask("?"), nil); err != nil {
		t.Fatal(err)
	}
	if body := f.body(0); body["thinking"] != nil || body["max_tokens"] != float64(999) || body["output_config"] != nil {
		t.Fatalf("no thinking %v", body)
	}
}

func TestImages(t *testing.T) {
	f, url := newFake(t, text("un gatto"))
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16))
	img, err := agent.Image(png)
	if err != nil {
		t.Fatal(err)
	}
	req := &agent.Request{Messages: []agent.Message{agent.UserMessage("cos'è?", img)}}
	if _, err := open(t, url, Config{}).Generate(t.Context(), req, nil); err != nil {
		t.Fatal(err)
	}
	if !has(f.sent(0), `"type":"image"`, `"media_type":"image/png"`, `"type":"base64"`, `"cos'è?"`) {
		t.Fatalf("sent %s", f.sent(0))
	}
}

// Input that is not a JSON object is the model's mistake: BadInput.
func TestBadInput(t *testing.T) {
	_, url := newFake(t, thinkingThenCall("s", "toolu_1", "lookup", `[1,2]`))
	got, err := open(t, url, Config{}).Generate(t.Context(), ask("?"), nil)
	if err != nil || !got.Message.Calls[0].BadInput || string(got.Message.Calls[0].Input) != "{}" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestStopReasons(t *testing.T) {
	for reason, want := range map[string]agent.StopReason{"end_turn": agent.StopEnd, "max_tokens": agent.StopMaxTokens,
		"refusal": agent.StopFiltered, "stop_sequence": agent.StopEnd, "model_context_window_exceeded": agent.StopMaxTokens, "pause_turn": agent.StopEnd} {
		answer := text("x")
		answer["stop_reason"] = reason
		_, url := newFake(t, answer)
		got, err := open(t, url, Config{}).Generate(t.Context(), ask("?"), nil)
		if err != nil || got.Stop != want {
			t.Errorf("%s: %+v %v", reason, got, err)
		}
	}
}

func TestErrorsAreClassified(t *testing.T) {
	for _, c := range []struct {
		status    int
		body      string
		code      errors.ErrorCode
		retryable bool
	}{
		{401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, agent.ErrProviderAuth, false},
		{429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`, agent.ErrProviderRate, true},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 2000000 tokens > 1000000 maximum"}}`, agent.ErrProviderContext, false},
		{413, `{"type":"error","error":{"type":"request_too_large","message":"big"}}`, agent.ErrProviderContext, false},
		{404, `{"type":"error","error":{"type":"not_found_error","message":"model: nope"}}`, agent.ErrProviderConfig, false},
		{529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, agent.ErrProviderUnavailable, true},
		{500, `{"type":"error","error":{"type":"api_error","message":"boom"}}`, agent.ErrProviderUnavailable, true},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"secret prompt echoed"}}`, agent.ErrProviderInput, false},
	} {
		f, url := newFake(t)
		f.status, f.errBody = c.status, c.body
		for _, sink := range []agent.TextSink{nil, func(string) error { return nil }} {
			_, err := open(t, url, Config{}).Generate(t.Context(), ask("?"), sink)
			classified(t, err, c.status, c.code, c.retryable)
		}
	}
}

func classified(t *testing.T, err error, status int, code errors.ErrorCode, retryable bool) {
	t.Helper()
	if !errors.HasCode(err, code) || isRetryable(err) != retryable || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "message") {
		t.Errorf("%d %s: %v", status, code, err)
	}
	if wait, ok := agent.RetryAfter(err); status == 429 && (!ok || wait != 7*time.Second) {
		t.Errorf("retry after %v %v", wait, ok)
	}
}

func TestSinkErrorIsReturnedAsIs(t *testing.T) {
	_, url := newFake(t, text("Parigi"))
	stop := errors.New("HOST_STOP", "the host stopped")
	_, err := open(t, url, Config{}).Generate(t.Context(), ask("?"), func(string) error { return stop })
	if err != stop {
		t.Fatalf("%v", err)
	}
}

func TestConfigRefused(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no model":    {APIKey: "k"},
		"no key":      {Model: "m"},
		"effort":      {Model: "m", APIKey: "k", Effort: "huge"},
		"negative":    {Model: "m", APIKey: "k", MaxTokens: -1},
		"http remote": {Model: "m", APIKey: "k", BaseURL: "http://gateway.example.com"},
		"query":       {Model: "m", APIKey: "k", BaseURL: "https://x.example.com/?a=b"},
		"user":        {Model: "m", APIKey: "k", BaseURL: "https://u:p@x.example.com"},
		"key control": {Model: "m", APIKey: "k\nX-Other: y"},
	} {
		if _, err := New(cfg); !errors.HasCode(err, agent.ErrProviderConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := New(Config{Model: "m", APIKey: "k", BaseURL: "http://gateway.lan:8080", AllowHTTP: true}); err != nil {
		t.Errorf("AllowHTTP: %v", err)
	}
}

func TestKeyFunc(t *testing.T) {
	f, url := newFake(t, text("a"))
	calls := 0
	p, err := New(Config{Model: "m", BaseURL: url, KeyFunc: func(context.Context) (string, error) { calls++; return "rotated", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Generate(t.Context(), ask("?"), nil); err != nil || calls != 1 || f.headers[0].Get("Authorization") != "Bearer rotated" {
		t.Fatalf("%v %d %q", err, calls, f.headers[0].Get("Authorization"))
	}
	for _, key := range []func(context.Context) (string, error){
		func(context.Context) (string, error) { return "", errors.New("VAULT", "sealed") },
		func(context.Context) (string, error) { return "", nil },
		func(context.Context) (string, error) { return "a\nb", nil },
	} {
		p, _ := New(Config{Model: "m", BaseURL: url, KeyFunc: key})
		if _, err := p.Generate(t.Context(), ask("?"), nil); !errors.HasCode(err, agent.ErrProviderAuth) {
			t.Errorf("%v", err)
		}
	}
}

func TestInfo(t *testing.T) {
	p, _ := New(Config{Model: "claude-haiku-5-5", APIKey: "k", ContextWindow: 1 << 20})
	if info := p.Info(); info.Name != "claude-haiku-5-5" || info.ContextWindow != 1<<20 {
		t.Fatalf("%+v", info)
	}
}

func isRetryable(err error) bool {
	var r errors.Retryable
	return stderrors.As(err, &r) && r.IsRetryable()
}

// A host's system message goes as one where the API takes it (after a
// user turn, before the model's), else as a marked note in a user turn.
func TestSystemMessages(t *testing.T) {
	f, url := newFake(t, text("ok"))
	msgs := []agent.Message{agent.SystemText("first note"), agent.UserText("ciao"), agent.SystemText("after the user"),
		agent.AssistantText("risposta"), agent.SystemText("before a user turn"), agent.UserText("e poi?"), agent.SystemText("last")}
	if _, err := open(t, url, Config{}).Generate(t.Context(), &agent.Request{Messages: msgs}, nil); err != nil {
		t.Fatal(err)
	}
	var sent []struct {
		Role    string           `json:"role"`
		Content []map[string]any `json:"content"`
	}
	_ = json.Unmarshal([]byte(f.sent(0)), &sent)
	roles := ""
	for _, m := range sent {
		roles += m.Role + " "
	}
	if roles != "user system assistant user system " {
		t.Fatalf("roles %q in %s", roles, f.sent(0))
	}
	if !has(f.sent(0), `system-reminder\u003e\nfirst note`, `system-reminder\u003e\nbefore a user turn`) {
		t.Fatalf("notes %s", f.sent(0))
	}
}

// A tool result, a host's note, then the user: one user turn, the result
// first (audit 21/11: two user turns in a row).
func TestSystemAfterAToolResult(t *testing.T) {
	f, url := newFake(t, text("ok"))
	calls := agent.Message{Role: agent.RoleAssistant, Calls: []agent.ToolCall{{ID: "toolu_1", Name: "t", Input: json.RawMessage(`{}`)}}}
	msgs := []agent.Message{agent.UserText("via"), calls, agent.ToolMessage(agent.ToolResult{CallID: "toolu_1", Content: "fatto"}),
		agent.SystemText("Your last reply was empty."), agent.UserText("allora?")}
	if _, err := open(t, url, Config{}).Generate(t.Context(), &agent.Request{Messages: msgs}, nil); err != nil {
		t.Fatal(err)
	}
	var sent []struct {
		Role    string           `json:"role"`
		Content []map[string]any `json:"content"`
	}
	_ = json.Unmarshal([]byte(f.sent(0)), &sent)
	if len(sent) != 3 || sent[2].Role != "user" || sent[2].Content[0]["type"] != "tool_result" || len(sent[2].Content) != 3 {
		t.Fatalf("%s", f.sent(0))
	}
}

// A tool's images go to Claude inside its result.
func TestToolResultImages(t *testing.T) {
	f, url := newFake(t, text("un grafico"))
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	calls := agent.Message{Role: agent.RoleAssistant, Calls: []agent.ToolCall{{ID: "toolu_1", Name: "shot", Input: json.RawMessage(`{}`)}}}
	res := agent.ToolResult{CallID: "toolu_1", Content: "screen", Images: []agent.Part{{Type: agent.PartImage, MIME: "image/png", Data: png}}}
	req := &agent.Request{Messages: []agent.Message{agent.UserText("guarda"), calls, agent.ToolMessage(res)}}
	if _, err := open(t, url, Config{}).Generate(t.Context(), req, nil); err != nil {
		t.Fatal(err)
	}
	if !has(f.sent(0), `"type":"tool_result"`, `"type":"image"`, `"media_type":"image/png"`, `"text":"screen"`) {
		t.Fatalf("%s", f.sent(0))
	}
}

// Structured output goes as output_config.format, beside the effort.
func TestOutputFormat(t *testing.T) {
	type answer struct {
		A string `json:"a"`
	}
	format, err := agent.FormatFor[answer]("answer")
	if err != nil {
		t.Fatal(err)
	}
	f, url := newFake(t, text(`{"a":"x"}`))
	req := ask("?")
	req.Format = format
	if _, err := open(t, url, Config{Effort: "low"}).Generate(t.Context(), req, nil); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(f.body(0))
	if !has(string(body), `"effort":"low"`, `"format":{"schema":{`, `"type":"json_schema"`, `"additionalProperties":false`) {
		t.Fatalf("body %s", body)
	}
}
