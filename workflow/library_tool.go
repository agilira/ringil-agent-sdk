// library_tool.go: the model saves workflows and runs saved ones
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package workflow

import (
	"context"
	"strconv"
	"strings"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Names of the library's tools.
const (
	SaveToolName     = "save_workflow"
	RunSavedToolName = "run_saved_workflow"
)

type saveIn struct {
	Name        string `json:"name" desc:"1-40 letters, digits, - and _; an existing name is replaced"`
	Description string `json:"description" desc:"What it does, in one sentence"`
	Brief       string `json:"brief,omitempty" desc:"Context every step needs: the goal, constraints, what is known"`
	Steps       []Step `json:"steps" desc:"The steps: name (lowercase), worker, skill (optional, a skill the worker follows), task (complete: the worker sees only it, the brief and the reports it needs), needs (names of earlier steps whose reports it uses)"`
	Output      string `json:"output" desc:"The step whose report is the result"`
}

// SaveTool lets the model save a workflow by name, for a trigger or the
// user to run again: workers are those of the crew that will run it. It
// counts as destructive, so an approval hook shows the whole plan before
// it is kept. saved, when set, hears of each one saved.
func (l *Library) SaveTool(crew Crew, saved func(Saved)) agent.Tool {
	desc := "Save a workflow by name, to run again later (from a trigger, or when the user asks): named steps, each a task for a worker (" +
		strings.Join(crew.Workers(), ", ") + "), optionally following a skill, with the earlier steps it needs. It is not run now."
	return agent.NewTool(SaveToolName, desc, func(_ context.Context, in saveIn) (string, error) {
		s := Saved{Name: in.Name, Description: in.Description, Brief: in.Brief, Plan: Plan{Steps: in.Steps, Output: in.Output}}
		if err := l.Put(s, crew.Has); err != nil {
			return "", agent.ToolError(ErrPlan, "Not saved: "+agent.ModelMessage(err))
		}
		if saved != nil {
			saved(s)
		}
		return "Workflow " + s.Name + " saved (" + strconv.Itoa(len(s.Plan.Steps)) + " steps). A trigger runs it by its name; run_saved_workflow runs it now.", nil
	}, agent.Destructive())
}

type runSavedIn struct {
	Name  string `json:"name" desc:"The saved workflow's name"`
	Input string `json:"input,omitempty" desc:"What this run is for, added to its brief as data"`
}

// RunSavedTool lets the model run a workflow of lib by name. It counts as
// destructive: the steps act.
func (r *Runner) RunSavedTool(lib *Library) agent.Tool {
	return agent.NewTool(RunSavedToolName, "Run a saved workflow by name, now. The user's saved workflows are listed by the host; ask the user which one when unsure.",
		func(ctx context.Context, in runSavedIn) (string, error) {
			s, err := lib.Get(in.Name)
			if err != nil {
				return "", agent.ToolError(ErrNotFound, agent.ModelMessage(err))
			}
			id := NewRunID(s.Name)
			res, err := r.RunSaved(ctx, id, s, in.Input)
			if err != nil {
				return "", agent.ToolError(ErrStep, "The workflow stopped: "+agent.ModelMessage(err)+" (id "+id+")."+r.partial(ctx, id))
			}
			return "Workflow " + id + " done.\n\nResult (" + s.Plan.Output + "):\n" + res.Output, nil
		}, agent.Destructive())
}
