// delegate.go: sub-agents the model hands work to
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package delegate lets an agent hand self-contained tasks to sub-agents.
// A sub-agent starts with a clean context: it sees only the task it is
// given, works with its own tools, and returns one final report, so long
// searches and reading stay out of the parent's context. Several
// sub-agents of one turn run at the same time.
//
// The host defines the kinds of worker (a researcher with read-only
// tools, a reviewer, a general worker), each with its own instructions
// and a subset of the tools. A worker never gets the delegate tool itself
// unless the host gives it: there is no unbounded nesting.
package delegate

import (
	"context"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// ToolName is the tool the model calls to delegate.
const ToolName = "delegate"

// ErrConfig marks a bad team.
const ErrConfig errors.ErrorCode = "DELEGATE_CONFIG"

// Worker is one kind of sub-agent.
type Worker struct {
	// Name is how the model picks it: lowercase letters, digits, -.
	Name string
	// Description tells the model when to use it.
	Description string
	// System is the worker's own instructions.
	System string
	// Tools the worker may use; nil means none.
	Tools agent.Registry
	// Provider overrides the team's model for this worker.
	Provider agent.Provider
	// MaxTurns bounds the worker's model calls; default the team's.
	MaxTurns int
	// Prepare, when set, runs before each task: per-task tools (a fresh
	// scratchpad) and context, and a function run when the task ends.
	Prepare func(ctx context.Context, task string) (Setup, error)
}

// Setup is what Prepare adds to one task.
type Setup struct {
	Tools   []agent.Tool
	Context func(ctx context.Context) (string, error)
	Done    func() error
}

// Team is the set of workers behind the delegate tool, and what they
// share with the parent.
type Team struct {
	Workers []Worker
	// Provider is the model workers use; normally the parent's, budget
	// included, so delegated work counts against the same limits.
	Provider agent.Provider
	// Approve is the parent's approval hook: a worker's risky calls are
	// confirmed like the parent's.
	Approve func(ctx context.Context, call agent.ToolCall, spec agent.ToolSpec) (bool, error)
	// MaxTurns bounds a worker's model calls; default 30.
	MaxTurns int
	// MaxReport is how much of a report the parent sees; default 32 KiB.
	MaxReport int
	// OnEvent, when set, receives every event of every worker, tagged
	// with the task's number, for a host that shows the work.
	OnEvent func(task int, worker string, ev agent.Event)
}

type taskIn struct {
	Worker       string `json:"worker,omitempty" desc:"Which kind of worker; not needed to continue a task"`
	Description  string `json:"description,omitempty" desc:"A few words naming the task, shown to the user"`
	Task         string `json:"task" desc:"The complete task: the worker sees nothing else, so include the goal, what you know, where to look and what to report back. To continue a task, your follow-up message"`
	Background   bool   `json:"background,omitempty" desc:"Start it and go on working; its report arrives later as a message"`
	ContinueTask int    `json:"continue_task,omitempty" min:"1" desc:"Number of a finished task to send a follow-up to; the worker keeps its context"`
}

// Tool returns the delegate tool for a new crew of t.
func Tool(t Team) (agent.Tool, error) {
	c, err := NewCrew(t)
	if err != nil {
		return nil, err
	}
	return c.Tool(), nil
}

// Tool is the crew's delegate tool.
func (c *Crew) Tool() agent.Tool {
	return agent.NewTool(ToolName, c.describe(), c.call, agent.Parallel())
}

func validName(n string) bool {
	if n == "" || len(n) > 32 {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func (c *Crew) describe() string {
	var b strings.Builder
	b.WriteString("Hand a self-contained task to a sub-agent with a clean context; it returns one final report, numbered. Use it for searches and work that would fill your context; call it several times in one turn to run tasks in parallel, or with background to keep working while it runs (its report then arrives as a message). Write the task in full: the worker knows nothing of this conversation. To ask a finished worker more, use continue_task with its number: it keeps its context. Workers:")
	for _, n := range c.names {
		b.WriteString("\n- " + n + ": " + c.byName[n].Description)
	}
	return b.String()
}

func (c *Crew) call(ctx context.Context, in taskIn) (string, error) {
	if strings.TrimSpace(in.Task) == "" {
		return "", agent.ToolError(agent.ErrToolInput, "The task is empty.")
	}
	t, err := c.task(ctx, in)
	if err != nil {
		return "", err
	}
	if in.Background {
		c.start(ctx, t, in.Task)
		return "Task " + strconv.Itoa(t.id) + " (" + t.worker.Name + ") started in the background; its report will arrive as a message. Go on with other work.", nil
	}
	defer c.release(t)
	report, err := c.work(ctx, t, in.Task)
	if err != nil {
		return "", err
	}
	return "Task " + strconv.Itoa(t.id) + " (" + t.worker.Name + "):\n" + report, nil
}

// task returns the task to run: a new one, or a finished one to continue.
func (c *Crew) task(ctx context.Context, in taskIn) (*task, error) {
	if in.ContinueTask > 0 {
		return c.resume(in.ContinueTask)
	}
	w, ok := c.byName[in.Worker]
	if !ok {
		return nil, agent.ToolError(agent.ErrToolInput, "No worker named "+strconv.Quote(in.Worker)+".")
	}
	setup, err := prepare(ctx, w, in.Task)
	if err != nil {
		return nil, err
	}
	return c.add(w, setup), nil
}

func prepare(ctx context.Context, w Worker, task string) (Setup, error) {
	if w.Prepare == nil {
		return Setup{}, nil
	}
	s, err := w.Prepare(ctx, task)
	if err != nil {
		return Setup{}, errors.Wrap(err, ErrConfig, "cannot prepare the worker")
	}
	return s, nil
}

// Run runs one task on a new worker of the given kind and returns its
// report: what the delegate tool does, for hosts and workflows.
func (c *Crew) Run(ctx context.Context, worker, taskText string) (string, error) {
	t, err := c.task(ctx, taskIn{Worker: worker, Task: taskText})
	if err != nil {
		return "", err
	}
	defer c.forget(t)
	return c.work(ctx, t, taskText)
}

// work runs one message of a task on its worker and returns the report.
func (c *Crew) work(ctx context.Context, t *task, message string) (string, error) {
	ctx = context.WithValue(ctx, infoKey{}, Info{Task: t.id, Worker: t.worker.Name})
	a, err := c.agentFor(t.worker, t.setup)
	if err != nil {
		return "", err
	}
	var res *agent.Result
	for ev, err := range a.Stream(ctx, t.conv, agent.UserText(message)) {
		if err != nil {
			return "", errors.Wrap(err, agent.ErrToolFailed, "the worker stopped").WithUserMessage("The worker stopped before finishing: " + agent.ModelMessage(err))
		}
		if c.OnEvent != nil {
			c.OnEvent(t.id, t.worker.Name, ev)
		}
		if ev.Kind == agent.EventDone {
			res = ev.Result
		}
	}
	if res == nil {
		return "", agent.ToolError(agent.ErrToolFailed, "The worker ended without a report.")
	}
	return c.report(res), nil
}

func (c *Crew) agentFor(w Worker, setup Setup) (agent.Agent, error) {
	tools := w.Tools
	if len(setup.Tools) > 0 {
		var err error
		if tools == nil {
			tools, err = agent.NewRegistry(setup.Tools...)
		} else {
			tools, err = tools.With(setup.Tools...)
		}
		if err != nil {
			return nil, errors.Wrap(err, ErrConfig, "the worker's tools clash")
		}
	}
	p := w.Provider
	if p == nil {
		p = c.Provider
	}
	return agent.New(agent.Config{
		Provider: p, Tools: tools, System: w.System, Context: setup.Context,
		Approve: c.Approve, MaxTurns: firstPositive(w.MaxTurns, c.MaxTurns, 30),
	})
}

// report is the worker's final text, bounded, with what it cost.
func (c *Crew) report(res *agent.Result) string {
	text := strings.TrimSpace(res.Text)
	if text == "" {
		text = "(the worker gave no final text)"
	}
	limit := firstPositive(c.MaxReport, 32<<10)
	if len(text) > limit {
		cut := limit
		for cut > 0 && text[cut]&0xC0 == 0x80 {
			cut--
		}
		text = text[:cut] + "\n[report cut]"
	}
	return text + "\n[worker: " + strconv.Itoa(res.Turns) + " model calls, " + strconv.Itoa(res.Usage.Input) + " in / " + strconv.Itoa(res.Usage.Output) + " out tokens; stop: " + string(res.Stop) + "]"
}

func firstPositive(vs ...int) int {
	for _, v := range vs {
		if v > 0 {
			return v
		}
	}
	return 0
}
