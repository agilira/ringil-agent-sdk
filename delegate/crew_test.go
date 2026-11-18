// crew_test.go: background tasks and follow-ups
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package delegate

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func callTool(t *testing.T, tool agent.Tool, in taskIn) (string, error) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Call(bg, raw)
	return out.Content, err
}

// A follow-up reaches the same worker with its context intact.
func TestContinueKeepsContext(t *testing.T) {
	worker := fake.New(fake.Reply("first report"), fake.Reply("second report"))
	crew, err := NewCrew(Team{Workers: []Worker{{Name: "w", Description: "w", Provider: worker}}})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()
	if out, err := callTool(t, crew.Tool(), taskIn{Worker: "w", Task: "look at A"}); err != nil || !strings.Contains(out, "Task 1") {
		t.Fatalf("first %q %v", out, err)
	}
	out, err := callTool(t, crew.Tool(), taskIn{ContinueTask: 1, Task: "and B?"})
	if err != nil || !strings.Contains(out, "second report") {
		t.Fatalf("continue %q %v", out, err)
	}
	checkContext(t, worker.Requests()[1].Messages)
	if _, err := callTool(t, crew.Tool(), taskIn{ContinueTask: 9, Task: "x"}); !strings.Contains(agent.ModelMessage(err), "No task 9") {
		t.Fatalf("unknown task: %v", err)
	}
}

func checkContext(t *testing.T, msgs []agent.Message) {
	t.Helper()
	if len(msgs) != 3 || msgs[0].Text() != "look at A" || msgs[2].Text() != "and B?" {
		t.Fatalf("worker context lost: %d messages", len(msgs))
	}
}

// A background task returns at once; its report comes through Next.
func TestBackgroundReportsLater(t *testing.T) {
	slowWorker := slow{fake.New(fake.Reply("background report")), 150 * time.Millisecond}
	crew, err := NewCrew(Team{Workers: []Worker{{Name: "w", Description: "w", Provider: slowWorker}}})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()
	start := time.Now()
	out, err := callTool(t, crew.Tool(), taskIn{Worker: "w", Task: "x", Background: true})
	if err != nil || !strings.Contains(out, "started in the background") || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("start %q %v after %v", out, err, time.Since(start))
	}
	checkLater(t, crew)
}

func checkLater(t *testing.T, crew *Crew) {
	t.Helper()
	if crew.Pending() != 1 {
		t.Fatalf("pending %d", crew.Pending())
	}
	f, ok := crew.Next(bg)
	if !ok || f.Task != 1 || !strings.Contains(f.Report, "background report") || crew.Pending() != 0 {
		t.Fatalf("next %+v %v", f, ok)
	}
	if text := Notice(f).Text(); !strings.Contains(text, "not a message from the user") {
		t.Fatalf("notice %q", text)
	}
	if _, ok := crew.Next(bg); ok {
		t.Fatal("Next with nothing pending")
	}
}

// Close stops a background task; its report says so.
func TestCloseStopsBackground(t *testing.T) {
	hang := slow{fake.New(fake.Reply("never")), 5 * time.Second}
	crew, err := NewCrew(Team{Workers: []Worker{{Name: "w", Description: "w", Provider: hangUntilDone{hang}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, crew.Tool(), taskIn{Worker: "w", Task: "x", Background: true}); err != nil {
		t.Fatal(err)
	}
	crew.Close()
	ctx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()
	if f, ok := crew.Next(ctx); !ok || f.Err == nil {
		t.Fatalf("closed task %+v %v", f, ok)
	}
}

// hangUntilDone waits for the context instead of sleeping blindly.
type hangUntilDone struct{ slow }

func (h hangUntilDone) Generate(ctx context.Context, _ *agent.Request, _ agent.TextSink) (*agent.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// Audit 18/11: Close returned before background work stopped and cleaned.
func TestCloseWaitsForBackground(t *testing.T) {
	var done atomic.Int32
	crew, err := NewCrew(Team{Workers: []Worker{{Name: "w", Description: "w", Provider: hangUntilDone{},
		Prepare: func(context.Context, string) (Setup, error) {
			return Setup{Done: func() error { done.Add(1); return nil }}, nil
		}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, crew.Tool(), taskIn{Worker: "w", Task: "x", Background: true}); err != nil {
		t.Fatal(err)
	}
	crew.Close()
	if done.Load() != 1 {
		t.Fatalf("cleanup after Close: %d", done.Load())
	}
}

// halfPanic answers once with usage, then panics.
type halfPanic struct{ calls atomic.Int32 }

func (h *halfPanic) Info() agent.ModelInfo { return agent.ModelInfo{Name: "h"} }
func (h *halfPanic) Generate(context.Context, *agent.Request, agent.TextSink) (*agent.Response, error) {
	if h.calls.Add(1) == 2 {
		panic("provider bug")
	}
	return &agent.Response{Message: agent.Message{Role: agent.RoleAssistant, Calls: []agent.ToolCall{{ID: "c1", Name: "noop", Input: []byte(`{}`)}}}, Usage: agent.Usage{Input: 100, Output: 10}, Stop: agent.StopToolUse}, nil
}

// Audit 18/11: a panic lost the spend of calls already paid.
func TestPanicKeepsSpend(t *testing.T) {
	noop := agent.NewTool("noop", "x", func(context.Context, struct{}) (string, error) { return "", nil }, agent.ReadOnly())
	tools, err := agent.NewRegistry(noop)
	if err != nil {
		t.Fatal(err)
	}
	crew, err := NewCrew(Team{Workers: []Worker{{Name: "w", Description: "w", Tools: tools, Provider: &halfPanic{}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()
	_, spent, err := crew.RunCounted(bg, "w", "x")
	if err == nil || spent.Calls != 1 || spent.Usage.Input != 100 {
		t.Fatalf("spent %+v err %v", spent, err)
	}
}

func TestWorkerTools(t *testing.T) {
	crew, err := NewCrew(Team{Workers: []Worker{{Name: "warehouse", Description: "knows stock", Provider: fake.New(fake.Reply("314 units"))}}})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()
	tools := crew.WorkerTools()
	if len(tools) != 1 || tools[0].Spec().Name != "warehouse" {
		t.Fatalf("tools %v", tools)
	}
	out, err := tools[0].Call(bg, json.RawMessage(`{"task":"stock of AX-7?"}`))
	if err != nil || !strings.Contains(out.Content, "314 units") {
		t.Fatalf("call %q %v", out.Content, err)
	}
}

// 18/11: a small model started its worker in the background and ended
// its turn; the report was lost. Converse waits and runs again on it.
func TestRunTakesBackgroundReports(t *testing.T) {
	slowWorker := slow{fake.New(fake.Reply("314 units")), 100 * time.Millisecond}
	crew, err := NewCrew(Team{Workers: []Worker{{Name: "w", Description: "w", Provider: slowWorker}}})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()
	parent := fake.New(fake.Call("w", `{"task":"stock?","background":true}`), fake.Reply("waiting"), fake.Reply("AX-7: 314"))
	reg, err := agent.NewRegistry(crew.WorkerTools()...)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: parent, Tools: reg})
	if err != nil {
		t.Fatal(err)
	}
	res, err := crew.Converse(bg, a, nil, agent.UserText("stock of AX-7?"))
	if err != nil || res.Text != "AX-7: 314" {
		t.Fatalf("%v %v", res, err)
	}
	if last := parent.Requests()[2].Messages; !strings.Contains(last[len(last)-1].Text(), "314 units") {
		t.Fatal("the report did not reach the parent")
	}
}

// 18/11: a workflow passed "[worker: 1 model calls, ...]" to the next
// step, and Qwen 3B summarized the cost line as the result. RunCounted
// returns the report alone; the delegating model still sees the cost.
func TestCostLineOnlyForTheDelegatingModel(t *testing.T) {
	crew, err := NewCrew(Team{Workers: []Worker{{Name: "w", Description: "w", Provider: fake.New(fake.Reply("314"), fake.Reply("314"))}}})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()
	report, spent, err := crew.RunCounted(bg, "w", "x")
	if err != nil || report != "314" || spent.Calls != 1 {
		t.Fatalf("%q %+v %v", report, spent, err)
	}
	out, err := callTool(t, crew.Tool(), taskIn{Worker: "w", Task: "x"})
	if err != nil || !strings.Contains(out, "[worker: 1 model calls") {
		t.Fatalf("%q %v", out, err)
	}
}
