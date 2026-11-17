// agent_test.go: tests for the agent loop
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"iter"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

type addIn struct {
	A int `json:"a"`
	B int `json:"b"`
}

func adder(calls *int) agent.Tool {
	return agent.NewTool("add", "Add two integers", func(_ context.Context, in addIn) (int, error) {
		*calls++
		return in.A + in.B, nil
	}, agent.ReadOnly())
}

func newAgent(t *testing.T, cfg agent.Config) agent.Agent {
	t.Helper()
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func tools(t *testing.T, ts ...agent.Tool) agent.Registry {
	t.Helper()
	r, err := agent.NewRegistry(ts...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func checkResult(t *testing.T, res *agent.Result, text string, turns, messages int) {
	t.Helper()
	if res.Text != text || res.Stop != agent.StopEnd || res.Turns != turns || len(res.Messages) != messages {
		t.Fatalf("result %+v", res)
	}
}

func TestNewValidates(t *testing.T) {
	cases := []agent.Config{
		{},
		{Provider: fake.New(), MaxTurns: -1},
		{Provider: fake.New(), MaxTokens: -1},
	}
	for i, cfg := range cases {
		if _, err := agent.New(cfg); !errors.HasCode(err, agent.ErrAgentConfig) {
			t.Errorf("case %d accepted: %v", i, err)
		}
	}
}

func TestPlainAnswer(t *testing.T) {
	p := fake.New(fake.Step{Chunks: []string{"cia", "o"}, Usage: agent.Usage{Input: 5, Output: 2}})
	res, err := newAgent(t, agent.Config{Provider: p}).Run(context.Background(), nil, agent.UserText("hi"))
	if err != nil {
		t.Fatal(err)
	}
	checkResult(t, res, "ciao", 1, 2)
	if res.Usage.Input != 5 {
		t.Fatalf("usage %+v", res.Usage)
	}
	req := p.Requests()[0]
	if req.System != agent.DefaultSystem || len(req.Tools) != 0 || req.Messages[0].Text() != "hi" {
		t.Fatalf("request %+v", req)
	}
}

func TestToolRoundTrip(t *testing.T) {
	calls := 0
	p := fake.New(fake.Call("add", `{"a":2,"b":3}`), fake.Reply("5"))
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls)), System: "custom"})
	res, err := a.Run(context.Background(), nil, agent.UserText("2+3?"))
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant with call, tool result, final assistant
	checkResult(t, res, "5", 2, 4)
	if calls != 1 || res.Messages[2].Result.Content != "5" || res.Messages[2].Result.CallID != res.Messages[1].Calls[0].ID {
		t.Fatalf("calls %d messages %+v", calls, res.Messages)
	}
	second := p.Requests()[1]
	if second.System != "custom" || len(second.Tools) != 1 || len(second.Messages) != 3 {
		t.Fatalf("second request %+v", second)
	}
}

func TestToolErrorsGoBackToTheModel(t *testing.T) {
	p := fake.New(fake.Call("add", `{"a":"x"}`), fake.Call("missing", `{}`), fake.Reply("sorry"))
	calls := 0
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))})
	hostErrs := toolErrors(t, a.Stream(context.Background(), nil, agent.UserText("go")))
	if calls != 0 || len(hostErrs) != 2 || !errors.HasCode(hostErrs[0], agent.ErrToolInput) || !errors.HasCode(hostErrs[1], agent.ErrToolUnknown) {
		t.Fatalf("calls %d errs %v", calls, hostErrs)
	}
	third := p.Requests()[2].Messages
	if !strings.Contains(third[2].Result.Content, "Invalid arguments") {
		t.Fatalf("model not told: %+v", third[2].Result)
	}
}

// toolErrors drains a run and returns the host errors of failed tool calls.
func toolErrors(t *testing.T, events iter.Seq2[agent.Event, error]) []error {
	t.Helper()
	var errs []error
	for ev, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind != agent.EventToolEnd {
			continue
		}
		if !ev.Output.IsError {
			t.Fatalf("failure not marked: %+v", ev.Output)
		}
		errs = append(errs, ev.Err)
	}
	return errs
}

func TestEventOrder(t *testing.T) {
	calls := 0
	p := fake.New(fake.Step{Chunks: []string{"let me add"}, Calls: []agent.ToolCall{{Name: "add", Input: json.RawMessage(`{"a":1,"b":1}`)}}}, fake.Reply("2"))
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))})
	var kinds []string
	for ev, err := range a.Stream(context.Background(), nil, agent.UserText("1+1")) {
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, string(ev.Kind))
	}
	want := "text,model,tool_start,tool_end,text,model,done"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("events %s\nwant   %s", strings.Join(kinds, ","), want)
	}
}

func TestMaxTurnsStopsCleanly(t *testing.T) {
	calls := 0
	p := fake.New(fake.Call("add", `{"a":1,"b":1}`), fake.Call("add", `{"a":1,"b":1}`), fake.Reply("never"))
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls)), MaxTurns: 2})
	res, err := a.Run(context.Background(), nil, agent.UserText("loop"))
	if err != nil || res.Stop != agent.StopMaxTurns || res.Turns != 2 || calls != 2 || p.Remaining() != 1 {
		t.Fatalf("res %+v err %v calls %d", res, err, calls)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != agent.RoleTool {
		t.Fatalf("history does not end at a complete turn: %+v", last)
	}
}

func TestHistoryIsUsedAndNotModified(t *testing.T) {
	history := []agent.Message{agent.UserText("earlier"), agent.AssistantText("ok")}
	p := fake.New(fake.Reply("again"))
	res, err := newAgent(t, agent.Config{Provider: p}).Run(context.Background(), history, agent.UserText("now"))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Requests()[0].Messages; len(got) != 3 || got[0].Text() != "earlier" {
		t.Fatalf("request messages %+v", got)
	}
	if len(res.Messages) != 2 || len(history) != 2 || history[1].Text() != "ok" {
		t.Fatalf("result %+v history %+v", res.Messages, history)
	}
}

func TestDuplicateCallIDsAreRenamed(t *testing.T) {
	calls := 0
	dup := agent.ToolCall{ID: "call_0", Name: "add", Input: json.RawMessage(`{"a":1,"b":2}`)}
	history := []agent.Message{
		agent.UserText("x"),
		{Role: agent.RoleAssistant, Calls: []agent.ToolCall{dup}},
		agent.ToolMessage(agent.ToolResult{CallID: "call_0", Content: "3"}),
		agent.AssistantText("3"),
	}
	p := fake.New(fake.Step{Calls: []agent.ToolCall{dup, dup}}, fake.Reply("done"))
	res, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))}).Run(context.Background(), history, agent.UserText("again"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{"call_0": true}
	for _, call := range res.Messages[1].Calls {
		if ids[call.ID] {
			t.Fatalf("duplicate id %q kept", call.ID)
		}
		ids[call.ID] = true
	}
	if res.Messages[2].Result.CallID != res.Messages[1].Calls[0].ID || res.Messages[3].Result.CallID != res.Messages[1].Calls[1].ID {
		t.Fatalf("results not paired: %+v", res.Messages)
	}
}

func TestProviderErrorEndsTheRun(t *testing.T) {
	boom := errors.New(agent.ErrProviderAuth, "401")
	_, err := newAgent(t, agent.Config{Provider: fake.New(fake.Fail(boom))}).Run(context.Background(), nil, agent.UserText("x"))
	if !errors.HasCode(err, agent.ErrProviderAuth) {
		t.Fatalf("err %v", err)
	}
}

func TestBreakingOutStopsTheRun(t *testing.T) {
	calls := 0
	p := fake.New(fake.Stream("a", "b", "c"), fake.Reply("unused"))
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))})
	seen := 0
	for ev, err := range a.Stream(context.Background(), nil, agent.UserText("x")) {
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if ev.Kind == agent.EventText {
			break
		}
	}
	if seen != 1 || p.Remaining() != 1 {
		t.Fatalf("seen %d remaining %d", seen, p.Remaining())
	}
}

// cancelling cancels the context while a tool runs.
func TestCancelDuringTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	slow := agent.NewTool("slow", "", func(context.Context, struct{}) (string, error) { cancel(); return "late", nil })
	p := fake.New(fake.Call("slow", `{}`), fake.Reply("unused"))
	_, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, slow)}).Run(ctx, nil, agent.UserText("x"))
	if !stderrors.Is(err, context.Canceled) || p.Remaining() != 1 {
		t.Fatalf("err %v remaining %d", err, p.Remaining())
	}
}

func TestMaxTokensPassedThrough(t *testing.T) {
	p := fake.New(fake.Reply("x"))
	if _, err := newAgent(t, agent.Config{Provider: p, MaxTokens: 77}).Run(context.Background(), nil, agent.UserText("x")); err != nil {
		t.Fatal(err)
	}
	if p.Requests()[0].MaxTokens != 77 {
		t.Fatal("MaxTokens not passed")
	}
}
