// events_test.go: one numbered sequence for the stream and the observers
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func sequence(evs []agent.Event) string {
	s := ""
	for _, ev := range evs {
		s += fmt.Sprintf("%d:%s ", ev.Seq, ev.Kind)
	}
	return s
}

// The stream and every observer see the same events, numbered from 1,
// under one run ID; Run (not only Stream) reaches the observers.
func TestOneSequence(t *testing.T) {
	slow := agent.NewTool("slow", "s", func(context.Context, struct{}) (string, error) {
		time.Sleep(5 * time.Millisecond)
		return "ok", nil
	})
	var seen []agent.Event
	cfg := agent.Config{Tools: tools(t, slow), Observers: []agent.Observer{func(ev agent.Event) { seen = append(seen, ev) }}}
	cfg.Provider = fake.New(fake.Call("slow", `{}`), fake.Reply("done"))
	var streamed []agent.Event
	for ev, err := range newAgent(t, cfg).Stream(context.Background(), nil, agent.UserText("x")) {
		if err != nil {
			t.Fatal(err)
		}
		streamed = append(streamed, ev)
	}
	if sequence(seen) != sequence(streamed) || seen[0].Kind != agent.EventStarted || seen[0].Seq != 1 {
		t.Fatalf("observer %s\nstream   %s", sequence(seen), sequence(streamed))
	}
	checkStamps(t, seen)
	// Run, with nobody reading the stream: the observer still sees it all,
	// under a new run ID.
	first := seen[0].Run
	seen = nil
	cfg.Provider = fake.New(fake.Reply("hi"))
	if _, err := newAgent(t, cfg).Run(context.Background(), nil, agent.UserText("x")); err != nil {
		t.Fatal(err)
	}
	if sequence(seen) != "1:started 2:text 3:model 4:done " || seen[0].Run == first {
		t.Fatalf("%s %s", sequence(seen), seen[0].Run)
	}
}

// checkStamps: one run ID, a time on every event, the tool's duration.
func checkStamps(t *testing.T, evs []agent.Event) {
	t.Helper()
	for _, ev := range evs {
		if ev.Run != evs[0].Run || ev.Run == "" || ev.Time.IsZero() {
			t.Fatalf("event %+v", ev)
		}
		if ev.Kind == agent.EventToolEnd && ev.Duration < 5*time.Millisecond {
			t.Fatalf("tool took %v", ev.Duration)
		}
	}
}

// A failed run ends with EventFailed, given to observers and, with the
// error, to the stream.
func TestFailureEndsTheSequence(t *testing.T) {
	var last agent.Event
	cfg := agent.Config{Provider: fake.New(fake.Fail(errors.New(agent.ErrProviderAuth, "401"))), Observers: []agent.Observer{func(ev agent.Event) { last = ev }}}
	var got agent.Event
	for ev, err := range newAgent(t, cfg).Stream(context.Background(), nil, agent.UserText("x")) {
		if err != nil {
			got = ev
		}
	}
	if last.Kind != agent.EventFailed || got.Kind != agent.EventFailed || got.Seq != last.Seq || !errors.HasCode(got.Err, agent.ErrProviderAuth) {
		t.Fatalf("observer %+v stream %+v", last, got)
	}
}

// An observer that panics is dropped; the run and the other observers go
// on.
func TestPanickingObserverIsDropped(t *testing.T) {
	calls, others := 0, 0
	bad := func(agent.Event) { calls++; panic("host bug") }
	good := func(agent.Event) { others++ }
	cfg := agent.Config{Provider: fake.New(fake.Call("add", `{"a":1,"b":2}`), fake.Reply("3")), Observers: []agent.Observer{bad, good}}
	n := 0
	cfg.Tools = tools(t, adder(&n))
	res, err := newAgent(t, cfg).Run(context.Background(), nil, agent.UserText("x"))
	if err != nil || res.Text != "3" || calls != 1 || others < 6 {
		t.Fatalf("res %v err %v bad %d good %d", res, err, calls, others)
	}
}

// Events of tools that run together are still numbered in one order.
func TestParallelCallsKeepOneOrder(t *testing.T) {
	var seqs []int
	cfg := agent.Config{Observers: []agent.Observer{func(ev agent.Event) { seqs = append(seqs, ev.Seq) }}}
	ro := agent.NewTool("ro", "r", func(context.Context, struct{}) (string, error) { return "x", nil }, agent.ReadOnly())
	cfg.Tools = tools(t, ro)
	calls := []agent.ToolCall{{Name: "ro", Input: json.RawMessage(`{}`)}, {Name: "ro", Input: json.RawMessage(`{}`)}, {Name: "ro", Input: json.RawMessage(`{}`)}}
	cfg.Provider = fake.New(fake.Step{Calls: calls}, fake.Reply("ok"))
	if _, err := newAgent(t, cfg).Run(context.Background(), nil, agent.UserText("x")); err != nil {
		t.Fatal(err)
	}
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("sequence %v", seqs)
		}
	}
}

// Audit 18/11 (1): a provider that answered the stopped consumer with an
// error of its own made the agent yield again: a panic in the caller.
type ownErr struct{}

func (ownErr) Info() agent.ModelInfo { return agent.ModelInfo{Name: "o"} }
func (ownErr) Generate(_ context.Context, _ *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	if err := sink("a"); err != nil {
		return nil, errors.New(agent.ErrProviderInput, "stream aborted")
	}
	return nil, nil
}

func TestNothingAfterTheConsumerStops(t *testing.T) {
	var kinds []agent.EventKind
	cfg := agent.Config{Provider: ownErr{}, Observers: []agent.Observer{func(ev agent.Event) { kinds = append(kinds, ev.Kind) }}}
	for ev, err := range newAgent(t, cfg).Stream(context.Background(), nil, agent.UserText("x")) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.EventText {
			break
		}
	}
	if fmt.Sprint(kinds) != "[started text]" {
		t.Fatalf("observer saw %v", kinds)
	}
}

// Audit 18/11 (2): a run refused by a busy conversation began with
// "failed"; EventStarted opens every run.
type busy struct{ agent.Conversation }

func (busy) BeginRun() (func(), error) { return nil, errors.New(agent.ErrSessionBusy, "busy") }

func TestStartedOpensEveryRun(t *testing.T) {
	var seen []agent.Event
	cfg := agent.Config{Provider: fake.New(fake.Reply("x")), Observers: []agent.Observer{func(ev agent.Event) { seen = append(seen, ev) }}}
	if _, err := newAgent(t, cfg).Run(context.Background(), busy{agent.NewConversation()}, agent.UserText("x")); err == nil {
		t.Fatal("busy conversation accepted")
	}
	if sequence(seen) != "1:started 2:failed " {
		t.Fatalf("%s", sequence(seen))
	}
}

// Audit 18/11 (3): the empty call before the nudge was counted in the
// result but in no EventModel, so a worker's spend was short.
func TestEveryModelCallIsReported(t *testing.T) {
	noop := agent.NewTool("noop", "n", func(context.Context, struct{}) (string, error) { return "", nil })
	sum := 0
	cfg := agent.Config{Tools: tools(t, noop), Provider: fake.New(fake.Step{Usage: agent.Usage{Input: 100}}, fake.Step{Chunks: []string{"hi"}, Usage: agent.Usage{Input: 7}}),
		Observers: []agent.Observer{func(ev agent.Event) {
			if ev.Kind == agent.EventModel {
				sum += ev.Usage.Input
			}
		}}}
	res, err := newAgent(t, cfg).Run(context.Background(), nil, agent.UserText("x"))
	if err != nil || sum != res.Usage.Input || sum != 107 {
		t.Fatalf("events %d result %d %v", sum, res.Usage.Input, err)
	}
}
