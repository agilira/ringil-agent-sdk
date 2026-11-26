// otel_test.go: the traces of a run
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package otel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

const secret = "s3cr3t-content"

type boomIn struct {
	Q string `json:"q"`
}

// traced runs one turn of an agent with a worker and a failing tool, and
// returns the spans it left.
func traced(t *testing.T) tracetest.SpanStubs {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	obs := Observer(tp.Tracer("test"), Options{Agent: "support"})

	model := fake.NewWithInfo(agent.ModelInfo{Name: "claude-test"},
		fake.Step{Calls: []agent.ToolCall{{Name: "delegate", Input: json.RawMessage(`{"worker":"explore","task":"` + secret + ` task"}`)}}, Usage: agent.Usage{Input: 100, Output: 10, Cost: 0.01}},
		fake.Step{Chunks: []string{secret + " report"}, Usage: agent.Usage{Input: 50, Output: 5, CacheRead: 40, Cost: 0.002}},
		fake.Step{Calls: []agent.ToolCall{{Name: "boom", Input: json.RawMessage(`{"q":"` + secret + `"}`)}}, Usage: agent.Usage{Input: 120, Output: 8, Cost: 0.012}},
		fake.Step{Chunks: []string{secret + " answer"}, Usage: agent.Usage{Input: 130, Output: 20, Cost: 0.015}},
	)
	crew, err := delegate.NewCrew(delegate.Team{
		Config:  agent.Config{Provider: model, Observers: []agent.Observer{obs}},
		Workers: []delegate.Worker{{Name: "explore", Description: "looks"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(crew.Close)
	boom := agent.NewTool("boom", "fails", func(context.Context, boomIn) (string, error) {
		return "", agent.ToolError("BOOM_DOWN", "the server at /srv/"+secret+" is down")
	}, agent.ReadOnly())
	reg, err := agent.NewRegistry(crew.Tool(), boom)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: reg, Observers: []agent.Observer{obs}, System: secret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), nil, agent.UserText(secret)); err != nil {
		t.Fatal(err)
	}
	return exp.GetSpans()
}

func byName(spans tracetest.SpanStubs, name string) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func attr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

// one is the only span named name.
func one(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	found := byName(spans, name)
	if len(found) != 1 {
		t.Fatalf("%d spans named %q", len(found), name)
	}
	return found[0]
}

// The tree: the run, its model calls and tools under it, the worker's run
// under the delegate call that started it, all in one trace.
func TestTraceTree(t *testing.T) {
	spans := traced(t)
	run, worker := one(t, spans, "invoke_agent support"), one(t, spans, "invoke_agent explore")
	delegated := one(t, spans, "execute_tool delegate")
	one(t, spans, "execute_tool boom")
	if n := len(byName(spans, "chat claude-test")); n != 4 {
		t.Fatalf("%d model calls", n)
	}
	if run.Parent.IsValid() || delegated.Parent.SpanID() != run.SpanContext.SpanID() || worker.Parent.SpanID() != delegated.SpanContext.SpanID() {
		t.Fatal("the worker's run is not under the delegate call, under the run")
	}
	for _, s := range spans {
		if s.SpanContext.TraceID() != run.SpanContext.TraceID() {
			t.Fatalf("%s is in another trace", s.Name)
		}
	}
	if attr(worker, "ringil.worker") != "explore" || attr(worker, "ringil.task") != "1" {
		t.Errorf("worker attrs %v", worker.Attributes)
	}
}

// The run carries its totals: tokens, dollars, turns.
func TestTraceRunTotals(t *testing.T) {
	run := one(t, traced(t), "invoke_agent support")
	if attr(run, "gen_ai.usage.input_tokens") != "350" || attr(run, "ringil.usage.cost_usd") != "0.037" || attr(run, "ringil.turns") != "3" {
		t.Errorf("run attrs %v", run.Attributes)
	}
}

// A model call carries its model, tokens and cost, within its own time.
func TestTraceModelCall(t *testing.T) {
	chat := byName(traced(t), "chat claude-test")[1] // the worker's
	if attr(chat, "gen_ai.usage.cache_read.input_tokens") != "40" || attr(chat, "gen_ai.response.model") != "claude-test" || attr(chat, "gen_ai.operation.name") != "chat" {
		t.Errorf("chat attrs %v", chat.Attributes)
	}
	if chat.EndTime.Before(chat.StartTime) {
		t.Error("a model call ends before it starts")
	}
}

// A failed tool is an error with its code, not its message.
func TestTraceToolError(t *testing.T) {
	boom := one(t, traced(t), "execute_tool boom")
	if boom.Status.Code != codes.Error || boom.Status.Description != "BOOM_DOWN" || attr(boom, "error.type") != "BOOM_DOWN" {
		t.Errorf("tool status %+v %v", boom.Status, boom.Attributes)
	}
}

// No content reaches a trace: prompts, answers, tool input, results and
// error messages stay out.
func TestTraceHasNoContent(t *testing.T) {
	for _, s := range traced(t) {
		dump, _ := json.Marshal(s.Attributes)
		events, _ := json.Marshal(s.Events)
		if strings.Contains(s.Name+string(dump)+string(events)+s.Status.Description, secret) {
			t.Fatalf("content in %s: %s %s", s.Name, dump, events)
		}
	}
}

// A failed run is an error span with its code; tool spans left open end
// with it.
func TestTraceFailedRun(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	obs := Observer(tp.Tracer("test"), Options{})
	a, err := agent.New(agent.Config{Provider: fake.New(fake.Fail(errors.New(agent.ErrProviderAuth, "key "+secret))), Observers: []agent.Observer{obs}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), nil, agent.UserText("x")); err == nil {
		t.Fatal("expected failure")
	}
	runs := byName(exp.GetSpans(), "invoke_agent")
	if len(runs) != 1 || runs[0].Status.Code != codes.Error || runs[0].Status.Description != string(agent.ErrProviderAuth) {
		t.Fatalf("%+v", exp.GetSpans())
	}
}

// Events of runs never started, or of a run already ended, are ignored;
// runs followed at once are bounded.
func TestTraceBounds(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	obs := Observer(tp.Tracer("test"), Options{})
	obs(agent.Event{Kind: agent.EventModel, Run: "ghost"})
	obs(agent.Event{Kind: agent.EventToolEnd, Run: "ghost", Call: &agent.ToolCall{ID: "1"}})
	obs(agent.Event{Kind: agent.EventDone, Run: "ghost"})
	for i := range maxRuns + 10 {
		obs(agent.Event{Kind: agent.EventStarted, Run: string(rune('a'+i%26)) + strings.Repeat("x", i)})
	}
	if n := len(exp.GetSpans()); n != 0 {
		t.Fatalf("%d spans ended", n)
	}
}

func TestTraceNilTracer(t *testing.T) {
	obs := Observer(nil, Options{Agent: "nil"})
	now := time.Now()
	obs(agent.Event{Kind: agent.EventStarted, Run: "r1", Time: now})
	obs(agent.Event{Kind: agent.EventModel, Run: "r1", Time: now, Model: "m"})
	obs(agent.Event{Kind: agent.EventToolStart, Run: "r1", Time: now, Call: &agent.ToolCall{ID: "c1", Name: "t"}})
	obs(agent.Event{Kind: agent.EventToolEnd, Run: "r1", Time: now, Call: &agent.ToolCall{ID: "c1", Name: "t"}})
	obs(agent.Event{Kind: agent.EventDone, Run: "r1", Time: now, Result: &agent.Result{Turns: 1}})
}

func TestTraceDuplicateStart(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	obs := Observer(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)).Tracer("t"), Options{})
	t0 := time.Unix(100, 0)
	obs(agent.Event{Kind: agent.EventStarted, Run: "r1", Time: t0})
	obs(agent.Event{Kind: agent.EventToolStart, Run: "r1", Time: t0, Call: &agent.ToolCall{ID: "leak", Name: "x"}})
	obs(agent.Event{Kind: agent.EventStarted, Run: "r1", Parent: "r1", Time: t0.Add(time.Second)})
	obs(agent.Event{Kind: agent.EventDone, Run: "r1", Time: t0.Add(2 * time.Second)})

	spans := exp.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3", len(spans))
	}
	if runs := byName(spans, "invoke_agent"); runs[1].Parent.IsValid() {
		t.Errorf("self-parented replacement run: %v", runs[1].Parent)
	}
	if tool := one(t, spans, "execute_tool x"); tool.Status.Code != codes.Error {
		t.Errorf("leaked tool status: %+v", tool.Status)
	}
}

func TestTraceEdgeCases(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	obs := Observer(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)).Tracer("t"), Options{})
	t0 := time.Unix(100, 0)
	obs(agent.Event{Kind: agent.EventStarted, Run: "r1", Time: t0})
	obs(agent.Event{Kind: agent.EventModel, Run: "r1", Model: "m", Duration: -5 * time.Second, Time: t0.Add(2 * time.Second)})
	obs(agent.Event{Kind: agent.EventToolStart, Run: "r1", Time: t0.Add(2 * time.Second), Call: &agent.ToolCall{ID: "c2", Name: "x"}})
	obs(agent.Event{Kind: agent.EventToolEnd, Run: "r1", Time: t0.Add(3 * time.Second), Call: &agent.ToolCall{ID: "c2", Name: "x"}, Err: errors.New("FAIL", "boom")})
	obs(agent.Event{Kind: agent.EventDone, Run: "r1", Time: t0.Add(4 * time.Second), Result: &agent.Result{Turns: 1}})

	spans := exp.GetSpans()
	if chat := one(t, spans, "chat m"); chat.EndTime.Before(chat.StartTime) {
		t.Errorf("negative duration shifted start after end: %v > %v", chat.StartTime, chat.EndTime)
	}
	if run := one(t, spans, "invoke_agent"); attr(run, "gen_ai.response.finish_reasons") != "" {
		t.Errorf("unexpected finish_reasons: %q", attr(run, "gen_ai.response.finish_reasons"))
	}
	if tool := one(t, spans, "execute_tool x"); tool.Status.Code != codes.Error || attr(tool, "error.type") != "FAIL" {
		t.Errorf("unexpected tool span: %+v %v", tool.Status, tool.Attributes)
	}
}

// Tools in parallel: two delegate calls and a quick read in the same
// turn. Each worker's run goes under the delegate call that started it,
// never under the other one or the read, which may end before they start.
func TestTraceParallelWorkers(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	obs := Observer(tp.Tracer("test"), Options{Agent: "support"})
	model := fake.NewWithInfo(agent.ModelInfo{Name: "m"},
		fake.Step{Calls: []agent.ToolCall{
			{ID: "d1", Name: "delegate", Input: json.RawMessage(`{"worker":"explore","task":"one"}`)},
			{ID: "d2", Name: "delegate", Input: json.RawMessage(`{"worker":"explore","task":"two"}`)},
			{ID: "q1", Name: "quick", Input: json.RawMessage(`{"q":"x"}`)},
		}},
		fake.Reply("report"), fake.Reply("report"), fake.Reply("done"),
	)
	crew, err := delegate.NewCrew(delegate.Team{Config: agent.Config{Provider: model, Observers: []agent.Observer{obs}}, Workers: []delegate.Worker{{Name: "explore", Description: "looks"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(crew.Close)
	quick := agent.NewTool("quick", "fast", func(context.Context, boomIn) (string, error) { return "ok", nil }, agent.ReadOnly())
	reg, err := agent.NewRegistry(crew.Tool(), quick)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: reg, Observers: []agent.Observer{obs}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), nil, agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	checkOwnParents(t, exp.GetSpans())
}

// checkOwnParents: each worker under a delegate call of its own.
func checkOwnParents(t *testing.T, spans tracetest.SpanStubs) {
	t.Helper()
	delegates := map[string]string{} // span ID → call ID
	for _, s := range byName(spans, "execute_tool delegate") {
		delegates[s.SpanContext.SpanID().String()] = attr(s, "gen_ai.tool.call.id")
	}
	workers := byName(spans, "invoke_agent explore")
	if len(workers) != 2 || len(delegates) != 2 {
		t.Fatalf("%d workers, %d delegate calls", len(workers), len(delegates))
	}
	seen := map[string]bool{}
	for _, w := range workers {
		call, ok := delegates[w.Parent.SpanID().String()]
		if !ok || seen[call] {
			t.Fatalf("a worker is not under its own delegate call: parent %s, calls %v", w.Parent.SpanID(), delegates)
		}
		seen[call] = true
	}
}
