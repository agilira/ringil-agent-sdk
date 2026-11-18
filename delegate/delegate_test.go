// delegate_test.go: sub-agents through the agent loop
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package delegate

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

var bg = context.Background()

type noIn struct{}

func lookup(found string, calls *atomic.Int32) agent.Tool {
	return agent.NewTool("lookup", "Look something up", func(context.Context, noIn) (string, error) {
		calls.Add(1)
		return found, nil
	}, agent.ReadOnly())
}

// slow delays every model answer, to see workers overlap.
type slow struct {
	agent.Provider
	d time.Duration
}

func (s slow) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	time.Sleep(s.d)
	return s.Provider.Generate(ctx, req, sink)
}

func parent(t *testing.T, team Team, steps ...fake.Step) (*agent.Result, fake.Script) {
	t.Helper()
	tool, err := Tool(team)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := agent.NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	p := fake.New(steps...)
	a, err := agent.New(agent.Config{Provider: p, Tools: reg})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(bg, nil, agent.UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	return res, p
}

func TestWorkerReportsBack(t *testing.T) {
	var calls atomic.Int32
	tools, err := agent.NewRegistry(lookup("the answer is 42", &calls))
	if err != nil {
		t.Fatal(err)
	}
	worker := fake.New(fake.Call("lookup", `{}`), fake.Reply("Found: 42."))
	var events atomic.Int32
	team := Team{Workers: []Worker{{Name: "research", Description: "Finds facts", System: "You research.", Tools: tools, Provider: worker}},
		OnEvent: func(int, string, agent.Event) { events.Add(1) }}
	res, p := parent(t, team, fake.Call(ToolName, `{"worker":"research","task":"Find the answer"}`), fake.Reply("It is 42."))
	report := p.Requests()[1].Messages[2].Result.Content
	if !strings.HasPrefix(report, "Found: 42.") || !strings.Contains(report, "[worker: 2 model calls") || calls.Load() != 1 || events.Load() == 0 {
		t.Fatalf("report %q calls %d events %d", report, calls.Load(), events.Load())
	}
	checkCleanContext(t, worker.Requests()[0])
	if res.Text != "It is 42." {
		t.Fatalf("parent %q", res.Text)
	}
}

// The worker saw only its task and its own instructions.
func checkCleanContext(t *testing.T, wreq *agent.Request) {
	t.Helper()
	if wreq.System != "You research." || len(wreq.Messages) != 1 || wreq.Messages[0].Text() != "Find the answer" {
		t.Fatalf("worker request %+v", wreq)
	}
}

func TestWorkersRunInParallel(t *testing.T) {
	mk := func() agent.Provider { return slow{fake.New(fake.Reply("done")), 200 * time.Millisecond} }
	team := Team{Workers: []Worker{{Name: "a", Description: "a", Provider: mk()}, {Name: "b", Description: "b", Provider: mk()}}}
	tool, err := Tool(team)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := agent.NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	calls := []agent.ToolCall{{Name: ToolName, Input: []byte(`{"worker":"a","task":"x"}`)}, {Name: ToolName, Input: []byte(`{"worker":"b","task":"y"}`)}}
	a, err := agent.New(agent.Config{Provider: fake.New(fake.Step{Calls: calls}, fake.Reply("ok")), Tools: reg})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := a.Run(bg, nil, agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 380*time.Millisecond {
		t.Fatalf("workers ran one after the other: %v", d)
	}
}

func TestPrepareApproveAndLimits(t *testing.T) {
	var asked, done atomic.Int32
	wipe := agent.NewTool("wipe", "Delete things", func(context.Context, noIn) (string, error) { return "wiped", nil }, agent.Destructive())
	tools, err := agent.NewRegistry(wipe)
	if err != nil {
		t.Fatal(err)
	}
	team := Team{
		Workers: []Worker{{Name: "w", Description: "w", Tools: tools, Provider: fake.New(fake.Call("wipe", `{}`), fake.Reply("refused, stopping")),
			Prepare: func(context.Context, string) (Setup, error) {
				return Setup{Done: func() error { done.Add(1); return nil }}, nil
			}}},
		Approve: func(_ context.Context, _ agent.ToolCall, spec agent.ToolSpec) (bool, error) {
			if spec.Annotations.Destructive {
				asked.Add(1)
				return false, nil
			}
			return true, nil
		},
	}
	_, p := parent(t, team, fake.Call(ToolName, `{"worker":"w","task":"clean up"}`), fake.Reply("ok"))
	if asked.Load() != 1 || done.Load() != 1 || !strings.Contains(p.Requests()[1].Messages[2].Result.Content, "refused") {
		t.Fatalf("asked %d done %d", asked.Load(), done.Load())
	}
}

func TestBadCalls(t *testing.T) {
	team := Team{Workers: []Worker{{Name: "w", Description: "w", Provider: fake.New()}}}
	_, p := parent(t, team, fake.Call(ToolName, `{"worker":"nope","task":"x"}`), fake.Call(ToolName, `{"worker":"w","task":"  "}`), fake.Reply("ok"))
	msgs := p.Requests()[2].Messages
	if !strings.Contains(msgs[2].Result.Content, "No worker") || !strings.Contains(msgs[4].Result.Content, "empty") {
		t.Fatalf("results %q / %q", msgs[2].Result.Content, msgs[4].Result.Content)
	}
	for _, bad := range []Team{{}, {Workers: []Worker{{Name: "Bad Name"}}}, {Workers: []Worker{{Name: "nomodel"}}}} {
		if _, err := Tool(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// Audit 18/11: cleanup skipped on a panic; hooks could not tell who asked.
func TestCleanupAfterPanicAndCurrent(t *testing.T) {
	var done atomic.Int32
	var who atomic.Value
	wipe := agent.NewTool("wipe", "x", func(context.Context, noIn) (string, error) { return "", nil }, agent.Destructive())
	tools, err := agent.NewRegistry(wipe)
	if err != nil {
		t.Fatal(err)
	}
	team := Team{
		Workers: []Worker{{Name: "w", Description: "w", Tools: tools, Provider: fake.New(fake.Call("wipe", `{}`), fake.Reply("ok")),
			Prepare: func(context.Context, string) (Setup, error) {
				return Setup{Done: func() error { done.Add(1); return nil }}, nil
			}}},
		Approve: func(ctx context.Context, _ agent.ToolCall, _ agent.ToolSpec) (bool, error) {
			if i, ok := Current(ctx); ok {
				who.Store(i.Worker)
			}
			return true, nil
		},
		OnEvent: func(_ int, _ string, ev agent.Event) {
			if ev.Kind == agent.EventDone {
				panic("host bug")
			}
		},
	}
	parent(t, team, fake.Call(ToolName, `{"worker":"w","task":"x"}`), fake.Reply("ok"))
	if done.Load() != 1 || who.Load() != "w" {
		t.Fatalf("done %d who %v", done.Load(), who.Load())
	}
}
