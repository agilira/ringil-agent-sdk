// otel.go: the runs of an agent as OpenTelemetry traces
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package otel records the runs of an agent as OpenTelemetry traces, by
// the GenAI semantic conventions: a span for each run (invoke_agent),
// and under it a span for each model call (chat, with the model, the
// tokens and the cost in US dollars) and each tool call (execute_tool).
// A delegated task's run goes under the call that started it.
//
// Spans carry metadata only, as package eventlog does: no prompt, answer,
// tool input or result, no error message (its code only), no tool name
// the model invented. The trace says what happened and what it cost, not
// what was said.
//
// The package needs only the OpenTelemetry API: the host sets up the SDK
// and its exporter (OTLP to Jaeger, Tempo, Honeycomb, Langfuse, ...) and
// passes its tracer:
//
//	agent.Config{Observers: []agent.Observer{otel.Observer(tp.Tracer("ringil"), otel.Options{Agent: "support"})}}
//
// Agents that delegate to one another (a crew, a server's profiles) share
// one Recorder, so that a task's run finds the call that started it:
//
//	rec := otel.New(tp.Tracer("ringil"))
//	lead := rec.Observer(otel.Options{Agent: "lead"})
package otel

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/eventlog"
)

// Attribute keys: the GenAI semantic conventions, and ours where they
// have none.
const (
	keyOperation  = attribute.Key("gen_ai.operation.name")
	keyAgent      = attribute.Key("gen_ai.agent.name")
	keyModel      = attribute.Key("gen_ai.response.model")
	keyInput      = attribute.Key("gen_ai.usage.input_tokens")
	keyOutput     = attribute.Key("gen_ai.usage.output_tokens")
	keyCacheRead  = attribute.Key("gen_ai.usage.cache_read.input_tokens")
	keyCacheWrite = attribute.Key("gen_ai.usage.cache_creation.input_tokens")
	keyFinish     = attribute.Key("gen_ai.response.finish_reasons")
	keyTool       = attribute.Key("gen_ai.tool.name")
	keyCallID     = attribute.Key("gen_ai.tool.call.id")
	keyErrorType  = attribute.Key("error.type")
	keyCost       = attribute.Key("ringil.usage.cost_usd")
	keyRun        = attribute.Key("ringil.run.id")
	keyWorker     = attribute.Key("ringil.worker")
	keyTask       = attribute.Key("ringil.task")
	keyTurns      = attribute.Key("ringil.turns")
)

// maxRuns bounds the runs followed at once: an observer that never saw a
// run's end (a host that dropped it) costs this much and no more.
// maxEnded bounds the runs, and maxEndedCalls a run's calls, kept once
// over: a background task starts after the call that started it, or its
// run, has ended, and goes under it all the same.
const (
	maxRuns       = 4096
	maxEnded      = 1024
	maxEndedCalls = 256
)

// Options shape the spans.
type Options struct {
	// Agent names the agent (gen_ai.agent.name), such as a profile; a
	// delegated task is named by its worker.
	Agent string
}

// Observer returns an observer that records each run on tracer: one
// agent's, or agents that do not delegate to each other (else see New).
// It is safe for concurrent use, as observers must be.
func Observer(tracer trace.Tracer, opts Options) agent.Observer {
	return New(tracer).Observer(opts)
}

// Recorder follows the runs of several agents on one tracer, so that a
// delegated task's run goes under the call of another agent that started
// it.
type Recorder struct {
	tracer trace.Tracer

	mu    sync.Mutex
	runs  map[string]*run
	ended map[string]*endedRun
	order []string // ended runs, oldest first
}

// New returns a Recorder on tracer.
func New(tracer trace.Tracer) *Recorder {
	if tracer == nil {
		tracer = noop.NewTracerProvider().Tracer("")
	}
	return &Recorder{tracer: tracer, runs: map[string]*run{}, ended: map[string]*endedRun{}}
}

// Observer is one agent's observer on the Recorder. It is safe for
// concurrent use, as observers must be.
func (r *Recorder) Observer(opts Options) agent.Observer {
	return func(ev agent.Event) { r.observe(opts.Agent, ev) }
}

// run is one run's open spans, and its calls already over.
type run struct {
	ctx   context.Context
	span  trace.Span
	tools map[string]trace.Span // by call ID: a delegated task's parent
	done  calls
}

// endedRun is a run over, kept as a parent for its late tasks.
type endedRun struct {
	span  trace.SpanContext
	calls calls
}

// calls are the span contexts of ended calls, by ID, the latest few.
type calls struct {
	by    map[string]trace.SpanContext
	order []string
}

func (c *calls) add(id string, sc trace.SpanContext) {
	if c.by == nil {
		c.by = map[string]trace.SpanContext{}
	}
	if _, seen := c.by[id]; !seen {
		c.order = append(c.order, id)
	}
	c.by[id] = sc
	if len(c.order) > maxEndedCalls {
		delete(c.by, c.order[0])
		c.order = c.order[1:]
	}
}

func (r *Recorder) observe(name string, ev agent.Event) {
	switch ev.Kind {
	case agent.EventStarted:
		r.start(name, ev)
	case agent.EventModel:
		r.model(ev)
	case agent.EventToolStart:
		r.toolStart(ev)
	case agent.EventToolEnd:
		r.toolEnd(ev)
	case agent.EventCompacted, agent.EventLoop, agent.EventRecalled, agent.EventRecovered, agent.EventContext:
		r.note(ev)
	case agent.EventDone, agent.EventFailed:
		r.end(ev)
	}
}

func (r *Recorder) start(agentName string, ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, dup := r.runs[ev.Run]; dup {
		closeTools(old, ev.Time)
		old.span.End(trace.WithTimestamp(ev.Time))
		delete(r.runs, ev.Run)
	}
	if len(r.runs) >= maxRuns {
		return
	}
	parent := r.parentCtx(ev)
	name, attrs := agentName, []attribute.KeyValue{keyOperation.String("invoke_agent"), keyRun.String(ev.Run)}
	if ev.Worker != "" {
		name = ev.Worker
		attrs = append(attrs, keyWorker.String(ev.Worker), keyTask.Int(ev.Task))
	}
	if name != "" {
		attrs = append(attrs, keyAgent.String(name))
	}
	ctx, span := r.tracer.Start(parent, spanName("invoke_agent", name),
		trace.WithTimestamp(ev.Time), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	r.runs[ev.Run] = &run{ctx: ctx, span: span, tools: map[string]trace.Span{}}
}

// parentCtx is where a run goes: under the call that started it, open or
// over, else under its parent run, else at the root.
func (r *Recorder) parentCtx(ev agent.Event) context.Context {
	if ev.Parent == "" || ev.Parent == ev.Run {
		return context.Background()
	}
	if p, ok := r.runs[ev.Parent]; ok {
		if call, open := p.tools[ev.ParentCall]; open {
			return trace.ContextWithSpan(p.ctx, call)
		}
		if sc, over := p.done.by[ev.ParentCall]; over {
			return trace.ContextWithSpanContext(context.Background(), sc)
		}
		return p.ctx
	}
	if p, ok := r.ended[ev.Parent]; ok {
		if sc, over := p.calls.by[ev.ParentCall]; over {
			return trace.ContextWithSpanContext(context.Background(), sc)
		}
		return trace.ContextWithSpanContext(context.Background(), p.span)
	}
	return context.Background()
}

// model is a model call: it happened in the Duration before the event.
func (r *Recorder) model(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ru, ok := r.runs[ev.Run]
	if !ok {
		return
	}
	attrs := append(usage(ev.Usage), keyOperation.String("chat"))
	if ev.Model != "" {
		attrs = append(attrs, keyModel.String(ev.Model))
	}
	if ev.Stop != "" {
		attrs = append(attrs, keyFinish.StringSlice([]string{eventlog.Stop(ev.Stop)}))
	}
	_, span := r.tracer.Start(ru.ctx, spanName("chat", ev.Model),
		trace.WithTimestamp(ev.Time.Add(-max(ev.Duration, 0))), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	span.End(trace.WithTimestamp(ev.Time))
}

func (r *Recorder) toolStart(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ru, ok := r.runs[ev.Run]
	if !ok || ev.Call == nil {
		return
	}
	name := eventlog.ToolName(ev)
	_, span := r.tracer.Start(ru.ctx, spanName("execute_tool", name), trace.WithTimestamp(ev.Time),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(keyOperation.String("execute_tool"), keyTool.String(name), keyCallID.String(eventlog.CallID(ev))))
	if old, open := ru.tools[ev.Call.ID]; open {
		old.End(trace.WithTimestamp(ev.Time)) // the same ID twice: the first is over
	}
	ru.tools[ev.Call.ID] = span
}

// toolEnd ends a call's span when the call ended: its events come once
// its whole batch has run. The span began when the call was asked for
// (its approval inside); an event marks when it began to run.
func (r *Recorder) toolEnd(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ru, ok := r.runs[ev.Run]
	if !ok || ev.Call == nil {
		return
	}
	span, open := ru.tools[ev.Call.ID]
	if !open {
		return
	}
	delete(ru.tools, ev.Call.ID)
	ru.done.add(ev.Call.ID, span.SpanContext())
	if code := toolError(ev); code != "" {
		span.SetAttributes(keyErrorType.String(code))
		span.SetStatus(codes.Error, code)
	}
	end := ev.Time
	if !ev.Began.IsZero() {
		span.AddEvent("ringil.tool.began", trace.WithTimestamp(ev.Began))
		end = ev.Began.Add(max(ev.Duration, 0))
	}
	span.End(trace.WithTimestamp(end))
}

// toolError is a failed call's error.type: its code, or tool_error for a
// tool that answered with an error.
func toolError(ev agent.Event) string {
	switch {
	case ev.Err != nil:
		return eventlog.Code(ev.Err)
	case ev.Output != nil && ev.Output.IsError:
		return "tool_error"
	}
	return ""
}

// note is a step that has no duration of its own: an event on the run's
// span, with the agent's own words about it.
func (r *Recorder) note(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ru, ok := r.runs[ev.Run]
	if !ok {
		return
	}
	attrs := usage(ev.Usage)
	if what := eventlog.Note(ev); what != "" {
		attrs = append(attrs, attribute.String("ringil.note", what))
	}
	if ev.Err != nil {
		attrs = append(attrs, keyErrorType.String(eventlog.Code(ev.Err)))
	}
	ru.span.AddEvent(string(ev.Kind), trace.WithTimestamp(ev.Time), trace.WithAttributes(attrs...))
}

func (r *Recorder) end(ev agent.Event) {
	r.mu.Lock()
	ru, ok := r.runs[ev.Run]
	delete(r.runs, ev.Run)
	if ok {
		r.keepEnded(ev.Run, ru)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	closeTools(ru, ev.Time)
	if ev.Result != nil {
		ru.span.SetAttributes(usage(ev.Result.Usage)...)
		ru.span.SetAttributes(keyTurns.Int(ev.Result.Turns))
		if ev.Result.Stop != "" {
			ru.span.SetAttributes(keyFinish.StringSlice([]string{eventlog.Stop(ev.Result.Stop)}))
		}
	}
	if ev.Kind == agent.EventFailed {
		code := eventlog.Code(ev.Err)
		ru.span.SetAttributes(keyErrorType.String(code))
		ru.span.SetStatus(codes.Error, code)
	}
	ru.span.End(trace.WithTimestamp(ev.Time))
}

// keepEnded keeps a run over as a parent for its late tasks, the latest
// maxEnded. Its open calls count as over.
func (r *Recorder) keepEnded(id string, ru *run) {
	e := &endedRun{span: ru.span.SpanContext(), calls: ru.done}
	for cid, s := range ru.tools {
		e.calls.add(cid, s.SpanContext())
	}
	if _, seen := r.ended[id]; !seen {
		r.order = append(r.order, id)
	}
	r.ended[id] = e
	if len(r.order) > maxEnded {
		delete(r.ended, r.order[0])
		r.order = r.order[1:]
	}
}

func closeTools(ru *run, at time.Time) {
	for _, open := range ru.tools { // a run that failed or restarted mid-call
		open.SetStatus(codes.Error, "run ended")
		open.End(trace.WithTimestamp(at))
	}
}

// usage is u as attributes, those that are not zero.
func usage(u agent.Usage) []attribute.KeyValue {
	var out []attribute.KeyValue
	for _, kv := range []struct {
		key attribute.Key
		n   int
	}{{keyInput, u.Input}, {keyOutput, u.Output}, {keyCacheRead, u.CacheRead}, {keyCacheWrite, u.CacheWrite}} {
		if kv.n > 0 {
			out = append(out, kv.key.Int(kv.n))
		}
	}
	if u.Cost > 0 {
		out = append(out, keyCost.Float64(u.Cost))
	}
	return out
}

// spanName is "<operation> <name>", or the operation alone.
func spanName(op, name string) string {
	if name == "" {
		return op
	}
	return op + " " + name
}
