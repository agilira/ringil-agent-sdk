// shared_test.go: agents on one Recorder, late tasks, a call's real time
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package otel

import (
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	agent "github.com/agilira/ringil-agent-sdk"
)

func recorded(t *testing.T) (*Recorder, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	return New(tp.Tracer("test")), exp
}

var t0 = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

func call(id string) *agent.ToolCall { return &agent.ToolCall{ID: id, Name: "delegate"} }

// A worker's run, seen by another agent's observer on the same Recorder,
// goes under the call that started it.
func TestSharedRecorderLinksAgents(t *testing.T) {
	rec, exp := recorded(t)
	lead, worker := rec.Observer(Options{Agent: "lead"}), rec.Observer(Options{Agent: "crew"})
	lead(agent.Event{Kind: agent.EventStarted, Run: "p", Time: at(0)})
	lead(agent.Event{Kind: agent.EventToolStart, Run: "p", Call: call("c1"), Time: at(1)})
	worker(agent.Event{Kind: agent.EventStarted, Run: "w", Parent: "p", ParentCall: "c1", Worker: "explore", Time: at(2)})
	worker(agent.Event{Kind: agent.EventDone, Run: "w", Time: at(3)})
	lead(agent.Event{Kind: agent.EventToolEnd, Run: "p", Call: call("c1"), Time: at(4)})
	lead(agent.Event{Kind: agent.EventDone, Run: "p", Time: at(5)})
	spans := exp.GetSpans()
	w, c := one(t, spans, "invoke_agent explore"), one(t, spans, "execute_tool delegate")
	if w.Parent.SpanID() != c.SpanContext.SpanID() || one(t, spans, "invoke_agent lead").Name == "" {
		t.Fatalf("the worker is not under the call: %v", w.Parent)
	}
}

// A background task starts after its call, or its whole run, ended: it
// goes under that call all the same.
func TestLateTaskGoesUnderItsCall(t *testing.T) {
	rec, exp := recorded(t)
	obs := rec.Observer(Options{Agent: "lead"})
	obs(agent.Event{Kind: agent.EventStarted, Run: "p", Time: at(0)})
	obs(agent.Event{Kind: agent.EventToolStart, Run: "p", Call: call("c1"), Time: at(1)})
	obs(agent.Event{Kind: agent.EventToolEnd, Run: "p", Call: call("c1"), Time: at(2)})
	obs(agent.Event{Kind: agent.EventStarted, Run: "w1", Parent: "p", ParentCall: "c1", Worker: "one", Time: at(3)})
	obs(agent.Event{Kind: agent.EventToolStart, Run: "p", Call: call("c2"), Time: at(4)})
	obs(agent.Event{Kind: agent.EventDone, Run: "p", Time: at(5)}) // c2 still open
	obs(agent.Event{Kind: agent.EventStarted, Run: "w2", Parent: "p", ParentCall: "c2", Worker: "two", Time: at(6)})
	obs(agent.Event{Kind: agent.EventStarted, Run: "w3", Parent: "p", ParentCall: "gone", Worker: "three", Time: at(7)})
	for _, w := range []string{"w1", "w2", "w3"} {
		obs(agent.Event{Kind: agent.EventDone, Run: w, Time: at(8)})
	}
	spans := exp.GetSpans()
	calls := byName(spans, "execute_tool delegate")
	if len(calls) != 2 {
		t.Fatalf("%d calls", len(calls))
	}
	for name, want := range map[string]string{
		"invoke_agent one":   calls[0].SpanContext.SpanID().String(),
		"invoke_agent two":   calls[1].SpanContext.SpanID().String(),
		"invoke_agent three": one(t, spans, "invoke_agent lead").SpanContext.SpanID().String(),
	} {
		s := one(t, spans, name)
		if s.Parent.SpanID().String() != want || s.Parent.TraceID() != calls[0].SpanContext.TraceID() {
			t.Errorf("%s: parent %s, want %s", name, s.Parent.SpanID(), want)
		}
	}
}

// The ended runs kept as parents are bounded.
func TestEndedRunsBounded(t *testing.T) {
	rec, _ := recorded(t)
	obs := rec.Observer(Options{})
	for i := range maxEnded + 10 {
		id := string(rune('a'+i%26)) + time.Duration(i).String()
		obs(agent.Event{Kind: agent.EventStarted, Run: id, Time: at(0)})
		obs(agent.Event{Kind: agent.EventDone, Run: id, Time: at(1)})
	}
	if len(rec.ended) != maxEnded || len(rec.order) != maxEnded {
		t.Fatalf("%d ended kept", len(rec.ended))
	}
}

// A call's span ends when the call did, not when its batch was recorded;
// a tool that answered an error is tool_error.
func TestToolSpanEndsWithTheCall(t *testing.T) {
	rec, exp := recorded(t)
	obs := rec.Observer(Options{})
	obs(agent.Event{Kind: agent.EventStarted, Run: "p", Time: at(0)})
	obs(agent.Event{Kind: agent.EventToolStart, Run: "p", Call: call("c1"), Time: at(1)})
	obs(agent.Event{Kind: agent.EventToolEnd, Run: "p", Call: call("c1"), Time: at(9), Began: at(2), Duration: time.Second,
		Output: &agent.ToolResult{CallID: "c1", IsError: true}})
	obs(agent.Event{Kind: agent.EventDone, Run: "p", Time: at(10)})
	s := one(t, exp.GetSpans(), "execute_tool delegate")
	if !s.EndTime.Equal(at(3)) || len(s.Events) != 1 || !s.Events[0].Time.Equal(at(2)) {
		t.Fatalf("end %v, events %v", s.EndTime, s.Events)
	}
	if attr(s, "error.type") != "tool_error" {
		t.Fatalf("error.type %q", attr(s, "error.type"))
	}
}
