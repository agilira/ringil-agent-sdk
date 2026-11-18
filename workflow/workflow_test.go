// workflow_test.go: plans, parallel steps, journal and resume
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/delegate"
)

var bg = context.Background()

// echo is a model that answers with the task it received, after a pause,
// counting how many answer at once.
type echo struct {
	mu      sync.Mutex
	now     int
	peak    int
	fail    string // a task containing this fails
	pause   time.Duration
	started []string
}

func (e *echo) Info() agent.ModelInfo { return agent.ModelInfo{Name: "echo"} }

func (e *echo) Generate(ctx context.Context, req *agent.Request, _ agent.TextSink) (*agent.Response, error) {
	task := req.Messages[0].Text()
	e.mu.Lock()
	e.now++
	e.peak = max(e.peak, e.now)
	e.started = append(e.started, strings.SplitN(task, "\n", 2)[0])
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.now--; e.mu.Unlock() }()
	select {
	case <-time.After(e.pause):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if e.fail != "" && strings.Contains(task, e.fail) {
		return nil, errors.New(agent.ErrProviderUnavailable, "down")
	}
	return &agent.Response{Message: agent.AssistantText("report of " + task), Stop: agent.StopEnd}, nil
}

func runner(t *testing.T, model agent.Provider, j Journal) *Runner {
	t.Helper()
	crew, err := delegate.NewCrew(delegate.Team{Provider: model, Workers: []delegate.Worker{{Name: "w", Description: "w"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(crew.Close)
	return &Runner{Crew: crew, Journal: j}
}

var diamond = Plan{Steps: []Step{
	{Name: "a", Worker: "w", Task: "A"},
	{Name: "b", Worker: "w", Task: "B", Needs: []string{"a"}},
	{Name: "c", Worker: "w", Task: "C", Needs: []string{"a"}},
	{Name: "d", Worker: "w", Task: "D", Needs: []string{"b", "c"}},
}, Output: "d"}

func TestPlanChecks(t *testing.T) {
	bad := []Plan{
		{},
		{Steps: []Step{{Name: "a", Worker: "w", Task: "x"}}, Output: "z"},
		{Steps: []Step{{Name: "a", Worker: "w", Task: "x"}, {Name: "a", Worker: "w", Task: "y"}}, Output: "a"},
		{Steps: []Step{{Name: "a", Worker: "w", Task: "x", Needs: []string{"b"}}, {Name: "b", Worker: "w", Task: "y", Needs: []string{"a"}}}, Output: "a"},
		{Steps: []Step{{Name: "a", Worker: "nobody", Task: "x"}}, Output: "a"},
		{Steps: []Step{{Name: "A B", Worker: "w", Task: "x"}}, Output: "A B"},
	}
	for i, p := range bad {
		if err := p.Check(func(w string) bool { return w == "w" }); !errors.HasCode(err, ErrPlan) {
			t.Errorf("plan %d accepted: %v", i, err)
		}
	}
	if err := diamond.Check(nil); err != nil {
		t.Fatal(err)
	}
}

func TestRunsInOrderAndInParallel(t *testing.T) {
	model := &echo{pause: 80 * time.Millisecond}
	res, err := runner(t, model, nil).Run(bg, "x", diamond, "the brief", false)
	if err != nil {
		t.Fatal(err)
	}
	if model.peak != 2 || model.started[0] != "A" || model.started[3] != "D" {
		t.Fatalf("peak %d order %v", model.peak, model.started)
	}
	if !strings.Contains(res.Output, "report of D") || !strings.Contains(res.Output, "the brief") || !strings.Contains(res.Output, `"b":`) {
		t.Fatalf("output %q", res.Output)
	}
}

// memJournal is a Journal in memory, copying states as a file would.
type memJournal struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (j *memJournal) Load(_ context.Context, id string) (*State, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	data, ok := j.m[id]
	if !ok {
		return nil, nil
	}
	var st State
	return &st, json.Unmarshal(data, &st)
}

func (j *memJournal) Save(_ context.Context, id string, st *State) error {
	data, err := json.Marshal(st)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.m == nil {
		j.m = map[string][]byte{}
	}
	j.m[id] = data
	return err
}

func TestResumeSkipsDoneAndStopsOnUncertain(t *testing.T) {
	j := &memJournal{}
	// First run: c fails; a and b are done, c is left pending.
	if _, err := runner(t, &echo{fail: "C"}, j).Run(bg, "r1", diamond, "", false); !errors.HasCode(err, ErrStep) {
		t.Fatalf("first run: %v", err)
	}
	if _, err := runner(t, &echo{}, j).Run(bg, "r1", diamond, "", false); !errors.HasCode(err, ErrExists) {
		t.Fatalf("rerun without resume: %v", err)
	}
	if _, err := runner(t, &echo{}, j).Run(bg, "r1", diamond, "", true); !errors.HasCode(err, ErrUncertain) {
		t.Fatalf("resume with a pending step: %v", err)
	}
	if err := Resolve(bg, j, "r1", "c", "checked by hand: C done"); err != nil {
		t.Fatal(err)
	}
	checkResume(t, j)
	other := diamond
	other.Output = "c"
	if _, err := runner(t, &echo{}, j).Run(bg, "r1", other, "", true); !errors.HasCode(err, ErrChanged) {
		t.Fatalf("changed plan: %v", err)
	}
}

// checkResume: only D runs, with C's resolved report among its inputs.
func checkResume(t *testing.T, j Journal) {
	t.Helper()
	model := &echo{}
	res, err := runner(t, model, j).Run(bg, "r1", diamond, "", true)
	if err != nil || len(model.started) != 1 || model.started[0] != "D" || !strings.Contains(res.Output, "checked by hand") {
		t.Fatalf("resume ran %v: %v %v", model.started, res, err)
	}
}

func TestFileJournalAndLock(t *testing.T) {
	j, err := NewFileJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner(t, &echo{}, j).Run(bg, "f1", diamond, "b", false); err != nil {
		t.Fatal(err)
	}
	st, err := j.Load(bg, "f1")
	if err != nil || st.Steps["d"].Status != Done {
		t.Fatalf("journal %+v %v", st, err)
	}
	unlock, err := j.Lock(bg, "f2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner(t, &echo{}, j).Run(bg, "f2", diamond, "", false); !errors.HasCode(err, ErrLocked) {
		t.Fatalf("locked run: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Load(bg, "../x"); err == nil {
		t.Fatal("bad id accepted")
	}
}

func TestToolRunsAPlan(t *testing.T) {
	r := runner(t, &echo{}, nil)
	raw, err := json.Marshal(planIn{Brief: "b", Steps: diamond.Steps, Output: "d"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Tool().Call(bg, raw)
	if err != nil || !strings.Contains(out.Content, "report of D") || !r.Tool().Spec().Annotations.Destructive {
		t.Fatalf("tool %q %v", out.Content, err)
	}
	bad, err := json.Marshal(planIn{Steps: []Step{{Name: "a", Worker: "w", Task: "x", Needs: []string{"a"}}}, Output: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Tool().Call(bg, bad); !strings.Contains(agent.ModelMessage(err), "cycle") {
		t.Fatalf("bad plan: %v", err)
	}
}
