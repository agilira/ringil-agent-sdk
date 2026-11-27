// hooks_test.go: the host's hooks around a run, its model calls and tools
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// BeforeRun changes the input the run records and the model reads.
func TestHookBeforeRunChanges(t *testing.T) {
	p := fake.New(fake.Reply("ok"))
	conv := agent.NewConversation()
	hooks := agent.Hooks{BeforeRun: func(_ context.Context, in agent.Message) (agent.Message, error) {
		return agent.UserText(strings.ReplaceAll(in.Text(), "secret", "[x]")), nil
	}}
	if _, err := newAgent(t, agent.Config{Provider: p, Hooks: hooks}).Run(context.Background(), conv, agent.UserText("my secret")); err != nil {
		t.Fatal(err)
	}
	if got := conv.Messages()[0].Text(); got != "my [x]" {
		t.Fatalf("recorded %q", got)
	}
	if got := p.Requests()[0].Messages[0].Text(); got != "my [x]" {
		t.Fatalf("the model read %q", got)
	}
}

// BeforeRun refusing, or panicking, ends the run before anything is
// recorded or asked; AfterRun is told the error.
func TestHookBeforeRunStops(t *testing.T) {
	for name, hook := range map[string]func(context.Context, agent.Message) (agent.Message, error){
		"error": func(context.Context, agent.Message) (agent.Message, error) {
			return agent.Message{}, stderrors.New("no")
		},
		"panic": func(context.Context, agent.Message) (agent.Message, error) { panic("host bug") },
	} {
		t.Run(name, func(t *testing.T) {
			p := fake.New(fake.Reply("never"))
			conv := agent.NewConversation()
			var told error
			hooks := agent.Hooks{BeforeRun: hook, AfterRun: func(_ context.Context, res *agent.Result, err error) {
				if res != nil {
					t.Error("a result for a run that did not start")
				}
				told = err
			}}
			_, err := newAgent(t, agent.Config{Provider: p, Hooks: hooks}).Run(context.Background(), conv, agent.UserText("x"))
			if !errors.HasCode(err, agent.ErrAgentHook) || !errors.HasCode(told, agent.ErrAgentHook) {
				t.Fatalf("run %v, AfterRun told %v", err, told)
			}
			if len(conv.Messages()) != 0 || len(p.Requests()) != 0 {
				t.Fatalf("%d messages recorded, %d requests", len(conv.Messages()), len(p.Requests()))
			}
		})
	}
}

// stopAtTurn is a BeforeModel that records the turns it sees and stops
// the run at turn stop.
func stopAtTurn(t *testing.T, stop int, turns *[]int) func(context.Context, agent.ModelCall) error {
	return func(_ context.Context, call agent.ModelCall) error {
		*turns = append(*turns, call.Turn)
		if call.Request == nil || len(call.Request.Tools) == 0 {
			t.Error("the request is not shown")
		}
		if call.Turn == stop {
			return stderrors.New("budget of the host")
		}
		return nil
	}
}

// BeforeModel sees every model call, numbered, and can stop the run: what
// was recorded stays.
func TestHookBeforeModel(t *testing.T) {
	calls := 0
	p := fake.New(fake.Call("add", `{"a":1,"b":2}`), fake.Reply("never"))
	var turns []int
	conv := agent.NewConversation()
	cfg := agent.Config{Provider: p, Tools: tools(t, adder(&calls)), Hooks: agent.Hooks{BeforeModel: stopAtTurn(t, 1, &turns)}}
	_, err := newAgent(t, cfg).Run(context.Background(), conv, agent.UserText("x"))
	if !errors.HasCode(err, agent.ErrAgentHook) {
		t.Fatalf("err %v", err)
	}
	if fmt.Sprint(turns) != "[0 1]" || len(p.Requests()) != 1 || calls != 1 {
		t.Fatalf("turns %v, %d requests, %d calls", turns, len(p.Requests()), calls)
	}
	if n := len(conv.Messages()); n != 3 { // input, call, result
		t.Fatalf("%d messages kept", n)
	}
}

// BeforeTool's Input is what Approve judges and the tool runs; the model's
// message keeps its own.
func TestHookBeforeToolChangesInput(t *testing.T) {
	calls := 0
	p := fake.New(fake.Call("add", `{"a":1,"b":2}`), fake.Reply("ok"))
	var approved string
	hooks := agent.Hooks{BeforeTool: func(_ context.Context, call agent.ToolCall, spec agent.ToolSpec) (agent.ToolCall, error) {
		if spec.Name != "add" {
			t.Errorf("spec %q", spec.Name)
		}
		call.Input = []byte(`{"a":10,"b":20}`)
		call.Name, call.ID = "forged", "forged" // only Input counts
		return call, nil
	}}
	approve := func(_ context.Context, call agent.ToolCall, _ agent.ToolSpec) (agent.Decision, error) {
		approved = string(call.Input)
		return agent.Allow(), nil
	}
	res, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls)), Hooks: hooks, Approve: approve}).Run(context.Background(), nil, agent.UserText("x"))
	if err != nil {
		t.Fatal(err)
	}
	if approved != `{"a":10,"b":20}` || res.Messages[2].Result.Content != "30" {
		t.Fatalf("approved %s, result %q", approved, res.Messages[2].Result.Content)
	}
	if got := string(res.Messages[1].Calls[0].Input); got != `{"a":1,"b":2}` || res.Messages[1].Calls[0].Name != "add" {
		t.Fatalf("the model's message became %s", got)
	}
}

// BeforeTool refusing, or panicking, refuses the call without asking
// Approve or running it; the run goes on.
func TestHookBeforeToolRefuses(t *testing.T) {
	for name, hook := range map[string]func(context.Context, agent.ToolCall, agent.ToolSpec) (agent.ToolCall, error){
		"error": func(context.Context, agent.ToolCall, agent.ToolSpec) (agent.ToolCall, error) {
			return agent.ToolCall{}, agent.ToolError("POLICY", "Not outside office hours.")
		},
		"panic": func(context.Context, agent.ToolCall, agent.ToolSpec) (agent.ToolCall, error) { panic("host bug") },
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			asked := false
			p := fake.New(fake.Call("add", `{"a":1,"b":2}`), fake.Reply("ok"))
			approve := func(context.Context, agent.ToolCall, agent.ToolSpec) (agent.Decision, error) {
				asked = true
				return agent.Allow(), nil
			}
			res, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls)), Hooks: agent.Hooks{BeforeTool: hook}, Approve: approve}).
				Run(context.Background(), nil, agent.UserText("x"))
			if err != nil {
				t.Fatal(err)
			}
			got := res.Messages[2].Result
			if calls != 0 || asked || !got.IsError || res.Text != "ok" {
				t.Fatalf("calls %d, asked %v, result %+v, text %q", calls, asked, got, res.Text)
			}
			want := map[string]string{"error": "Not outside office hours.", "panic": "the application failed to check it"}[name]
			if !strings.Contains(got.Content, want) {
				t.Fatalf("the model was told %q", got.Content)
			}
		})
	}
}

// AfterRun is told the result once, before the last event goes out.
func TestHookAfterRunBeforeDone(t *testing.T) {
	var told atomic.Int32
	var seenAtDone int32 = -1
	hooks := agent.Hooks{AfterRun: func(_ context.Context, res *agent.Result, err error) {
		if err != nil || res == nil || res.Text != "ok" {
			t.Errorf("told %+v %v", res, err)
		}
		res.Text = "changed by the hook" // a copy: the run's result stays
		told.Add(1)
	}}
	obs := func(ev agent.Event) {
		if ev.Kind == agent.EventDone {
			seenAtDone = told.Load()
		}
	}
	res, err := newAgent(t, agent.Config{Provider: fake.New(fake.Reply("ok")), Hooks: hooks, Observers: []agent.Observer{obs}}).
		Run(context.Background(), nil, agent.UserText("x"))
	if err != nil || res.Text != "ok" {
		t.Fatalf("%v %+v", err, res)
	}
	if told.Load() != 1 || seenAtDone != 1 {
		t.Fatalf("told %d times, %d at EventDone", told.Load(), seenAtDone)
	}
}

// A run whose reader stops is told to AfterRun as stopped; a provider's
// failure with its own error.
func TestHookAfterRunFailures(t *testing.T) {
	var got []error
	hooks := agent.Hooks{AfterRun: func(_ context.Context, _ *agent.Result, err error) { got = append(got, err) }}
	a := newAgent(t, agent.Config{Provider: fake.New(fake.Reply("one two three")), Hooks: hooks})
	for ev, err := range a.Stream(context.Background(), nil, agent.UserText("x")) {
		if err != nil || ev.Kind == agent.EventText {
			break
		}
	}
	failing := fake.New(fake.Step{Err: errors.New(agent.ErrProviderInput, "refused")})
	if _, err := newAgent(t, agent.Config{Provider: failing, Hooks: hooks}).Run(context.Background(), nil, agent.UserText("x")); err == nil {
		t.Fatal("no error")
	}
	if len(got) != 2 || !errors.HasCode(got[0], agent.ErrAgentStopped) || !errors.HasCode(got[1], agent.ErrProviderInput) {
		t.Fatalf("told %v", got)
	}
}

// A panicking AfterRun changes nothing.
func TestHookAfterRunPanics(t *testing.T) {
	bad := agent.Hooks{AfterRun: func(context.Context, *agent.Result, error) { panic("host bug") }}
	res, err := newAgent(t, agent.Config{Provider: fake.New(fake.Reply("ok")), Hooks: bad}).Run(context.Background(), nil, agent.UserText("x"))
	if err != nil || res.Text != "ok" {
		t.Fatalf("%v %+v", err, res)
	}
}
