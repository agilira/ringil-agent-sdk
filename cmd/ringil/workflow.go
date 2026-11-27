// workflow.go: background reports and workflows in the terminal agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/workflow"
)

// reports hands finished background tasks to the agent, one turn each.
// With wait (one-shot run) it waits for all of them; in a chat it takes
// those already finished and says how many still run.
func (c *cli) reports(ctx context.Context, a agent.Agent, conv agent.Conversation, wait bool) error {
	if c.crew == nil {
		return nil
	}
	for c.crew.Pending() > 0 {
		next := ctx
		if !wait {
			done, cancel := context.WithCancel(ctx)
			cancel() // take only what has already finished
			next = done
		}
		f, ok := c.crew.Next(next)
		if !ok {
			break
		}
		c.note("background task " + strconv.Itoa(f.Task) + " (" + f.Worker + ") finished")
		if err := c.turnWith(ctx, a, conv, delegate.Notice(f)); err != nil {
			return err
		}
	}
	if n := c.crew.Pending(); n > 0 && !wait {
		c.note(strconv.Itoa(n) + " background task(s) still running; their reports come with your next message")
	}
	return nil
}

func (c *cli) workflowDir() (string, error) {
	base, err := c.configDir()
	if err != nil {
		return "", errors.Wrap(err, ErrUsage, "no user config directory")
	}
	return filepath.Join(base, "ringil", "workflows"), nil
}

// workflowTool lets the agent run plans it writes, journaled so that an
// interrupted one can be resumed with `ringil workflow resume`.
func (c *cli) workflowTool(ctx *orpheus.Context) (agent.Tool, error) {
	r, err := c.budgetedRunner(ctx)
	if err != nil {
		return nil, err
	}
	return r.Tool(), nil
}

// budgetedRunner is the runner with the budget of the settings.
func (c *cli) budgetedRunner(ctx *orpheus.Context) (*workflow.Runner, error) {
	r, err := c.runner()
	if err != nil {
		return nil, err
	}
	if r.Budget, err = c.workflowBudget(ctx); err != nil {
		return nil, err
	}
	return r, c.checkLimits("workflow-cost", r.Budget.Cost)
}

// workflowBudget: what one workflow may spend in total, across resumes.
func (c *cli) workflowBudget(ctx *orpheus.Context) (workflow.Budget, error) {
	usd, err := c.flagDollars(ctx, "workflow-cost")
	return workflow.Budget{Calls: max(0, c.flagInt(ctx, "workflow-calls")), Tokens: max(0, c.flagInt(ctx, "workflow-tokens")), Cost: usd}, err
}

func (c *cli) runner() (*workflow.Runner, error) {
	dir, err := c.workflowDir()
	if err != nil {
		return nil, err
	}
	j, err := workflow.NewFileJournal(dir)
	if err != nil {
		return nil, err
	}
	return &workflow.Runner{Crew: c.crew, Journal: j, OnStep: func(step, status string, err error) {
		line := "    [step " + step + "] " + status
		if err != nil {
			line += ": " + describe(err)
		}
		c.note(strings.TrimPrefix(line, "  "))
	}}, nil
}

// showPlan prints a workflow the agent wants to run, before the question.
func (c *cli) showPlan(call agent.ToolCall) {
	var in struct {
		Brief  string          `json:"brief"`
		Steps  []workflow.Step `json:"steps"`
		Output string          `json:"output"`
	}
	if json.Unmarshal(call.Input, &in) != nil {
		return
	}
	c.note("\n  Workflow of " + strconv.Itoa(len(in.Steps)) + " steps (result: " + short(in.Output) + "):")
	if in.Brief != "" {
		c.note("    brief, given to every step: " + short(in.Brief))
	}
	for _, s := range in.Steps {
		needs := ""
		if len(s.Needs) > 0 {
			needs = " after " + short(strings.Join(s.Needs, ", "))
		}
		c.note("    " + short(s.Name) + " [" + short(s.Worker) + "]" + needs + ": " + short(s.Task))
	}
}

func (c *cli) journal() (*workflow.FileJournal, error) {
	dir, err := c.workflowDir()
	if err != nil {
		return nil, err
	}
	return workflow.NewFileJournal(dir)
}

func (c *cli) cmdWorkflows(*orpheus.Context) error {
	j, err := c.journal()
	if err != nil {
		return err
	}
	ids, err := j.List()
	if err != nil || len(ids) == 0 {
		if err == nil {
			err = c.line("no workflows")
		}
		return err
	}
	for _, id := range ids {
		st, err := j.Load(context.Background(), id)
		if err != nil || st == nil {
			continue
		}
		if err := c.line(id + "  " + summary(st)); err != nil {
			return err
		}
	}
	return nil
}

func summary(st *workflow.State) string {
	done, pending := 0, 0
	for _, s := range st.Steps {
		switch s.Status {
		case workflow.Done:
			done++
		case workflow.Pending:
			pending++
		}
	}
	text := strconv.Itoa(done) + "/" + strconv.Itoa(len(st.Plan.Steps)) + " steps done, " +
		strconv.Itoa(st.Spent.Calls) + " model calls, " + strconv.Itoa(st.Spent.Usage.Input+st.Spent.Usage.Output) + " tokens"
	if st.Spent.Usage.Cost > 0 {
		text += ", " + usd(st.Spent.Usage.Cost)
	}
	if pending > 0 {
		text += ", " + strconv.Itoa(pending) + " uncertain (see: ringil workflow resolve)"
	}
	return text
}

// cmdWorkflowResume continues an interrupted workflow from its journal.
func (c *cli) cmdWorkflowResume(ctx *orpheus.Context) error {
	if len(ctx.Positional()) != 1 {
		return errors.New(ErrUsage, "name the workflow: ringil workflow resume <id>")
	}
	defer c.shutdown()
	if _, err := c.agent(ctx); err != nil { // builds the crew with its tools
		return err
	}
	if c.crew == nil {
		return errors.New(ErrUsage, "workflows need the agent's tools; drop --no-tools")
	}
	j, err := c.journal()
	if err != nil {
		return err
	}
	id := ctx.Positional()[0]
	st, err := j.Load(ctx.Context(), id)
	if err != nil || st == nil {
		return errors.New(ErrUsage, "no workflow "+id)
	}
	r, err := c.budgetedRunner(ctx)
	if err != nil {
		return err
	}
	res, err := r.Run(ctx.Context(), id, st.Plan, st.Brief, true)
	if err != nil {
		return err
	}
	_, err = io.WriteString(c.out, clean(res.Output)+"\n")
	return err
}

// cmdWorkflowResolve records the outcome of an uncertain step, read from
// standard input, after the user checked what it did.
func (c *cli) cmdWorkflowResolve(ctx *orpheus.Context) error {
	if len(ctx.Positional()) != 2 {
		return errors.New(ErrUsage, "ringil workflow resolve <id> <step>, with the step's outcome on standard input")
	}
	j, err := c.journal()
	if err != nil {
		return err
	}
	report, err := io.ReadAll(io.LimitReader(c.in, 64<<10))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(report)) == "" {
		return errors.New(ErrUsage, "write the step's verified outcome on standard input")
	}
	if err := workflow.Resolve(ctx.Context(), j, ctx.Positional()[0], ctx.Positional()[1], string(report)); err != nil {
		return err
	}
	return c.line("recorded; resume with: ringil workflow resume " + ctx.Positional()[0])
}

// cmdWorkflowRetry clears an uncertain step so that resume runs it again:
// for when the user checked that running it twice is safe.
func (c *cli) cmdWorkflowRetry(ctx *orpheus.Context) error {
	if len(ctx.Positional()) != 2 {
		return errors.New(ErrUsage, "ringil workflow retry <id> <step>")
	}
	j, err := c.journal()
	if err != nil {
		return err
	}
	if err := workflow.Retry(ctx.Context(), j, ctx.Positional()[0], ctx.Positional()[1]); err != nil {
		return err
	}
	return c.line("the step will run again; resume with: ringil workflow resume " + ctx.Positional()[0])
}

// cmdWorkflowRemove deletes workflows' journals: they hold plans, briefs
// and reports in clear.
func (c *cli) cmdWorkflowRemove(ctx *orpheus.Context) error {
	if len(ctx.Positional()) == 0 {
		return errors.New(ErrUsage, "name the workflows to delete (see: ringil workflow)")
	}
	j, err := c.journal()
	if err != nil {
		return err
	}
	for _, id := range ctx.Positional() {
		if err := j.Remove(ctx.Context(), id); err != nil {
			return err
		}
	}
	return nil
}
