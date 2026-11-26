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
const maxRuns = 4096

// Options shape the spans.
type Options struct {
	// Agent names the agent (gen_ai.agent.name), such as a profile; a
	// delegated task is named by its worker.
	Agent string
}

// Observer returns an observer that records each run on tracer. It is
// safe for concurrent use, as observers must be.
func Observer(tracer trace.Tracer, opts Options) agent.Observer {
	if tracer == nil {
		tracer = noop.NewTracerProvider().Tracer("")
	}
	r := &recorder{tracer: tracer, agent: opts.Agent, runs: map[string]*run{}}
	return r.observe
}

type recorder struct {
	tracer trace.Tracer
	agent  string

	mu   sync.Mutex
	runs map[string]*run
}

// run is one run's open spans.
type run struct {
	ctx   context.Context
	span  trace.Span
	tools map[string]trace.Span // by call ID: a delegated task's parent
}

func (r *recorder) observe(ev agent.Event) {
	switch ev.Kind {
	case agent.EventStarted:
		r.start(ev)
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

func (r *recorder) start(ev agent.Event) {
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
	name, attrs := r.agent, []attribute.KeyValue{keyOperation.String("invoke_agent"), keyRun.String(ev.Run)}
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

func (r *recorder) parentCtx(ev agent.Event) context.Context {
	if p, ok := r.runs[ev.Parent]; ok && ev.Parent != "" && ev.Parent != ev.Run {
		if call, open := p.tools[ev.ParentCall]; open {
			return trace.ContextWithSpan(p.ctx, call) // under the call that started it
		}
		return p.ctx
	}
	return context.Background()
}

// model is a model call: it happened in the Duration before the event.
func (r *recorder) model(ev agent.Event) {
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

func (r *recorder) toolStart(ev agent.Event) {
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

func (r *recorder) toolEnd(ev agent.Event) {
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
	if (ev.Output != nil && ev.Output.IsError) || ev.Err != nil {
		code := eventlog.Code(ev.Err)
		span.SetAttributes(keyErrorType.String(code))
		span.SetStatus(codes.Error, code)
	}
	span.End(trace.WithTimestamp(ev.Time))
}

// note is a step that has no duration of its own: an event on the run's
// span, with the agent's own words about it.
func (r *recorder) note(ev agent.Event) {
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

func (r *recorder) end(ev agent.Event) {
	r.mu.Lock()
	ru, ok := r.runs[ev.Run]
	delete(r.runs, ev.Run)
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
