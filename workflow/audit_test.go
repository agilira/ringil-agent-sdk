// audit_test.go: regressions for the workflow audit of 18/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

type panicker struct{ echo }

func (p *panicker) Generate(context.Context, *agent.Request, agent.TextSink) (*agent.Response, error) {
	panic("provider bug")
}

// Finding 1: a panic in a step killed the process.
func TestStepPanicFailsTheStep(t *testing.T) {
	if _, err := runner(t, &panicker{}, nil).Run(bg, "p", diamond, "", false); !errors.HasCode(err, ErrStep) {
		t.Fatalf("panic: %v", err)
	}
}

// Finding 3: Resolve ignored a live run's lock.
func TestResolveRespectsTheLock(t *testing.T) {
	j, err := NewFileJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner(t, &echo{fail: "B"}, j).Run(bg, "l", diamond, "", false); err == nil {
		t.Fatal("expected a failed step")
	}
	unlock, err := j.Lock(bg, "l")
	if err != nil {
		t.Fatal(err)
	}
	if err := Resolve(bg, j, "l", "b", "done by hand"); !errors.HasCode(err, ErrLocked) {
		t.Fatalf("resolve while locked: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	checkRetry(t, j)
}

// checkRetry: after Retry, the resume runs b again and what follows it,
// never a or c, which were done.
func checkRetry(t *testing.T, j *FileJournal) {
	t.Helper()
	if err := Retry(bg, j, "l", "b"); err != nil {
		t.Fatal(err)
	}
	model := &echo{}
	if _, err := runner(t, model, j).Run(bg, "l", diamond, "", true); err != nil || strings.Join(model.started, ",") != "B,D" {
		t.Fatalf("retry ran %v: %v", model.started, err)
	}
	if err := j.Remove(bg, "l"); err != nil {
		t.Fatal(err)
	}
	if st, err := j.Load(bg, "l"); err != nil || st != nil {
		t.Fatalf("removed run still there: %v", err)
	}
}

// Finding 4: a runner without a crew panicked.
func TestNilCrew(t *testing.T) {
	if _, err := (&Runner{}).Run(bg, "x", diamond, "", false); !errors.HasCode(err, ErrPlan) {
		t.Fatalf("nil crew: %v", err)
	}
}

// A cancel must not mark steps that never ran.
func TestCancelMarksNothingNew(t *testing.T) {
	j := &memJournal{}
	ctx, cancel := context.WithCancel(bg)
	model := &echo{}
	r := runner(t, model, j)
	r.OnStep = func(step, status string, _ error) {
		if step == "a" && status == "done" {
			cancel()
		}
	}
	if _, err := r.Run(ctx, "c", diamond, "", false); err == nil {
		t.Fatal("cancel not reported")
	}
	st, err := j.Load(bg, "c")
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range st.Steps {
		if name != "a" && s.Status == Pending {
			t.Fatalf("step %s marked pending after the cancel", name)
		}
	}
}
