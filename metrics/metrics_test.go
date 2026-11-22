// metrics_test.go: counting a real agent's runs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package metrics

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

type addIn struct{ A, B int }

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content type %q", ct)
	}
	return rec.Body.String()
}

func has(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("lacks %q in:\n%s", l, out)
		}
	}
}

// A real run: a tool call, two model calls, the tokens, the outcome.
func TestARunIsCounted(t *testing.T) {
	m := New()
	add := agent.NewTool("add", "adds", func(_ context.Context, in addIn) (int, error) { return in.A + in.B, nil })
	tools, err := agent.NewRegistry(add)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: fake.New(fake.Call("add", `{"A":1,"B":2}`), fake.Reply("3")), Tools: tools, Observers: []agent.Observer{m.Observer("writer")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), agent.NewConversation(), agent.UserText("1+2?")); err != nil {
		t.Fatal(err)
	}
	out := scrape(t, m)
	has(t, out,
		`ringil_runs_started_total{profile="writer"} 1`,
		`ringil_runs_active{profile="writer"} 0`,
		`ringil_runs_finished_total{profile="writer",status="done",reason="end"} 1`,
		`ringil_model_calls_total{profile="writer"} 2`,
		`ringil_tool_calls_total{profile="writer",tool="add",status="ok"} 1`,
		`ringil_model_call_seconds_count{profile="writer"} 2`,
		`ringil_tool_call_seconds_bucket{profile="writer",le="+Inf"} 1`,
		"# TYPE ringil_model_call_seconds histogram",
	)
}

func TestFailuresAndToolsAreClassified(t *testing.T) {
	m := New()
	obs := m.Observer("p")
	obs(agent.Event{Kind: agent.EventStarted, Run: "r1"})
	obs(agent.Event{Kind: agent.EventModel, Usage: agent.Usage{Input: 100, Output: 7, CacheRead: 60}, Duration: 3 * time.Second})
	obs(agent.Event{Kind: agent.EventToolEnd, Call: &agent.ToolCall{Name: "made_up"}, UnknownTool: true})
	obs(agent.Event{Kind: agent.EventToolEnd, Call: &agent.ToolCall{Name: "read"}, Output: &agent.ToolResult{IsError: true}})
	obs(agent.Event{Kind: agent.EventLoop, Text: "blocked"})
	obs(agent.Event{Kind: agent.EventToolEnd, Call: &agent.ToolCall{Name: "other"}})
	obs(agent.Event{Kind: agent.EventStarted, Run: "w1", Parent: "r1"})
	obs(agent.Event{Kind: agent.EventFailed, Run: "w1", Parent: "r1", Err: context.Canceled})
	obs(agent.Event{Kind: agent.EventFailed, Run: "r1", Err: errors.New(agent.ErrProviderRate, "429 from upstream: secret text")})
	out := scrape(t, m)
	has(t, out,
		`ringil_runs_started_total{profile="p"} 1`,
		`ringil_delegated_tasks_total{profile="p"} 1`,
		`ringil_runs_finished_total{profile="p",status="failed",reason="PROVIDER_RATE"} 1`,
		`ringil_tokens_total{profile="p",kind="input"} 100`,
		`ringil_tokens_total{profile="p",kind="cache_read"} 60`,
		`ringil_tool_calls_total{profile="p",tool="(unknown)",status="ok"} 1`,
		`ringil_tool_calls_total{profile="p",tool="read",status="error"} 1`,
		`ringil_loops_total{profile="p",action="blocked"} 1`,
		`ringil_tool_calls_total{profile="p",tool="other",status="ok"} 1`,
		`ringil_model_call_seconds_bucket{profile="p",le="2.5"} 0`,
		`ringil_model_call_seconds_bucket{profile="p",le="5"} 1`,
	)
	if strings.Contains(out, "cancelled") || strings.Contains(out, "secret") {
		t.Fatalf("a delegated task's end counted as a run, or an error's text:\n%s", out)
	}
}

func TestOutputIsStable(t *testing.T) {
	m := New()
	for _, p := range []string{"b", "a"} {
		m.Observer(p)(agent.Event{Kind: agent.EventStarted, Run: p})
	}
	first, second := scrape(t, m), scrape(t, m)
	if first != second || strings.Index(first, `profile="a"`) > strings.Index(first, `profile="b"`) {
		t.Fatal("output not sorted or not stable")
	}
	if _, err := m.WriteTo(io.Discard); err != nil {
		t.Fatal(err)
	}
}

// A provider of the host's may stop for any reason: the label stays one
// of the agent's.
func TestStopReasonsAreBounded(t *testing.T) {
	m := New()
	obs := m.Observer("p")
	for _, r := range []string{"end", "weird-1", "weird-2"} {
		obs(agent.Event{Kind: agent.EventStarted, Run: r})
		obs(agent.Event{Kind: agent.EventDone, Run: r, Stop: agent.StopReason(r)})
	}
	has(t, scrape(t, m), `ringil_runs_finished_total{profile="p",status="done",reason="end"} 1`, `ringil_runs_finished_total{profile="p",status="done",reason="other"} 2`)
}

// A run that panics or whose reader stops still ends: the gauge goes
// back (audit 22/11).
func TestEveryRunEnds(t *testing.T) {
	m := New()
	a, err := agent.New(agent.Config{Provider: fake.New(fake.Stream("a", "b"), fake.Reply("x")), Observers: []agent.Observer{m.Observer("p")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range a.Stream(context.Background(), nil, agent.UserText("hi")) {
		if err != nil {
			t.Fatal(err)
		}
		break // the reader stops after the first event
	}
	has(t, scrape(t, m), `ringil_runs_active{profile="p"} 0`, `ringil_runs_finished_total{profile="p",status="failed",reason="AGENT_STOPPED"} 1`)
}
