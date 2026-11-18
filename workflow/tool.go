// tool.go: workflows the agent writes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package workflow

import (
	"context"
	"crypto/rand"
	"sort"
	"strings"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
)

// ToolName is the tool the model calls to run a plan it wrote.
const ToolName = "run_workflow"

type planIn struct {
	Brief  string `json:"brief,omitempty" desc:"Context every step needs: the goal, constraints, what is known"`
	Steps  []Step `json:"steps" desc:"The steps: name (lowercase), worker, task (complete: the worker sees only it, the brief and the reports it needs), needs (names of earlier steps whose reports it uses)"`
	Output string `json:"output" desc:"The step whose report is the result"`
}

// Tool lets the model write a plan and run it: for work with several
// stages where some can run in parallel. It counts as destructive, so an
// approval hook shows the plan before it runs; each step's own risky
// calls are still confirmed one by one.
func (r *Runner) Tool() agent.Tool {
	desc := "Run a workflow you design: named steps, each a task for a worker (" + strings.Join(r.Crew.Workers(), ", ") + "), with the earlier steps it needs. Steps whose needs are done run in parallel; each worker sees only its task, the brief and the reports it needs. Use it for work with several stages; for one task use delegate."
	return agent.NewTool(ToolName, desc, r.runPlan, agent.Destructive())
}

func (r *Runner) runPlan(ctx context.Context, in planIn) (string, error) {
	id := "wf-" + time.Now().Format("20060102-150405") + "-" + strings.ToLower(rand.Text()[:6])
	res, err := r.Run(ctx, id, Plan{Steps: in.Steps, Output: in.Output}, in.Brief, false)
	if err != nil {
		return "", agent.ToolError(ErrStep, "The workflow stopped: "+err.Error()+" (id "+id+")."+r.partial(ctx, id))
	}
	var b strings.Builder
	b.WriteString("Workflow " + id + " done.\n\nResult (" + in.Output + "):\n" + res.Output)
	var names []string
	for n := range res.Reports {
		if n != in.Output {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		b.WriteString("\n\nOther steps done: " + strings.Join(names, ", "))
	}
	return b.String(), nil
}

// partial lists what a stopped run finished, so the model can tell the
// user what was done and that the run can be resumed.
func (r *Runner) partial(ctx context.Context, id string) string {
	if r.Journal == nil {
		return ""
	}
	st, err := r.Journal.Load(ctx, id)
	if err != nil || st == nil {
		return ""
	}
	var done, open []string
	for name, s := range st.Steps {
		if s.Status == Done {
			done = append(done, name)
		} else {
			open = append(open, name)
		}
	}
	sort.Strings(done)
	sort.Strings(open)
	text := ""
	if len(done) > 0 {
		text += " Steps done: " + strings.Join(done, ", ") + "."
	}
	if len(open) > 0 {
		text += " Started and not finished: " + strings.Join(open, ", ") + " (the user checks them before resuming)."
	}
	return text + " The workflow can be resumed."
}
