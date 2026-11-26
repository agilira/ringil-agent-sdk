// security_test.go: the tracer at its limits
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package otel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	agent "github.com/agilira/ringil-agent-sdk"
)

var kinds = []agent.EventKind{agent.EventStarted, agent.EventModel, agent.EventToolStart, agent.EventToolEnd,
	agent.EventCompacted, agent.EventLoop, agent.EventRecalled, agent.EventDone, agent.EventFailed, agent.EventText}

// FuzzEvents: events in any order, of any runs and calls, with content in
// every field, never panic and never put the content in a span.
func FuzzEvents(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 7, 0, 2, 2, 8})
	f.Add([]byte{3, 7, 7, 0, 0, 2, 5, 6})
	f.Fuzz(func(t *testing.T, plan []byte) {
		exp := tracetest.NewInMemoryExporter()
		obs := Observer(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)).Tracer("f"), Options{Agent: "a"})
		at := time.Unix(1, 0)
		for i, b := range plan {
			name := "read_file" // a tool the agent has: its name is metadata
			if b%5 == 0 {
				name = secret // a name the model invented: content
			}
			call := &agent.ToolCall{ID: string(rune('a' + b%3)), Name: name, Input: json.RawMessage(`"` + secret + `"`)}
			kind := kinds[int(b)%len(kinds)]
			text := secret
			if kind == agent.EventCompacted || kind == agent.EventRecalled || kind == agent.EventLoop {
				text = "3 memories" // the agent's own words about the step
			}
			obs(agent.Event{
				Kind: kind, Run: string(rune('r' + b%2)), Parent: string(rune('r' + (b+1)%2)),
				Time: at.Add(time.Duration(i) * time.Second), Duration: time.Duration(b) * time.Millisecond,
				Text: text, Call: call, UnknownTool: b%5 == 0, Model: "m",
				Output: &agent.ToolResult{Content: secret, IsError: b%2 == 0},
				Result: &agent.Result{Text: secret, Usage: agent.Usage{Input: int(b), Cost: float64(b) / 100}},
			})
		}
		for _, s := range exp.GetSpans() {
			dump, _ := json.Marshal(s.Attributes)
			events, _ := json.Marshal(s.Events)
			if strings.Contains(s.Name+string(dump)+string(events)+s.Status.Description, secret) {
				t.Fatalf("content in %s: %s %s", s.Name, dump, events)
			}
		}
	})
}
