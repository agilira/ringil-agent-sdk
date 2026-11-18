// crew_test.go: background tasks and follow-ups
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package delegate

import (
	"context"
	"encoding/json"
	"strings"
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
