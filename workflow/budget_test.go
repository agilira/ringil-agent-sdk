// budget_test.go: a workflow's spending, kept across resumes
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

func TestBudgetStopsAndResumeKeepsCounting(t *testing.T) {
	j := &memJournal{}
	r := runner(t, &echo{}, j)
	r.Budget = Budget{Calls: 2}
	if _, err := r.Run(bg, "b", diamond, "", false); !errors.HasCode(err, ErrBudget) {
		t.Fatalf("over budget: %v", err)
	}
	st, err := j.Load(bg, "b")
	if err != nil || st.Spent.Calls != 3 || st.Steps["d"] != nil {
		t.Fatalf("spent %+v, d %+v", st.Spent, st.Steps["d"])
	}
	checkBudgetResume(t, j)
}

// checkBudgetResume: with a larger budget only d runs, and the count goes
// on from the first attempt.
func checkBudgetResume(t *testing.T, j Journal) {
	t.Helper()
	model := &echo{}
	r := runner(t, model, j)
	r.Budget = Budget{Calls: 10}
	if _, err := r.Run(bg, "b", diamond, "", true); err != nil || strings.Join(model.started, ",") != "D" {
		t.Fatalf("resume ran %v: %v", model.started, err)
	}
	if st, err := j.Load(bg, "b"); err != nil || st.Spent.Calls != 4 || st.Steps["d"].Spent.Calls != 1 {
		t.Fatalf("spent after resume %+v", st.Spent)
	}
}

// A failed step stays pending, its spending saved: a failed model call
// reports no usage, so here nothing was spent.
func TestFailedStepIsSaved(t *testing.T) {
	j := &memJournal{}
	if _, err := runner(t, &echo{fail: "A"}, j).Run(bg, "f", diamond, "", false); err == nil {
		t.Fatal("expected failure")
	}
	if st, err := j.Load(bg, "f"); err != nil || st.Spent.Calls != 0 || st.Steps["a"].Status != Pending {
		t.Fatalf("state %+v %v", st, err)
	}
}

// Audit 18/11: a run whose last step reached the budget exactly was
// reported as over it, though complete.
func TestBudgetReachedByTheLastStep(t *testing.T) {
	r := runner(t, &echo{}, nil)
	r.Budget = Budget{Calls: 1}
	one := Plan{Steps: []Step{{Name: "a", Worker: "w", Task: "A"}}, Output: "a"}
	if res, err := r.Run(bg, "x", one, "", false); err != nil || res == nil {
		t.Fatalf("complete run: %v", err)
	}
}

// costly adds a cost to every answer of next.
type costly struct {
	agent.Provider
	usd float64
}

func (c costly) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	resp, err := c.Provider.Generate(ctx, req, sink)
	if resp != nil {
		resp.Usage = agent.Usage{Input: 10, Output: 10, Cost: c.usd}
	}
	return resp, err
}

// A dollar budget stops the run like the others, and the journal keeps
// the dollars for the resume.
func TestBudgetInDollars(t *testing.T) {
	j := &memJournal{}
	r := runner(t, costly{&echo{}, 0.5}, j)
	r.Budget = Budget{Cost: 1}
	if _, err := r.Run(bg, "usd", diamond, "", false); !errors.HasCode(err, ErrBudget) {
		t.Fatalf("over budget: %v", err)
	}
	st, err := j.Load(bg, "usd")
	if err != nil || st.Spent.Usage.Cost != 1.5 || st.Steps["d"] != nil {
		t.Fatalf("spent %+v", st.Spent)
	}
}
