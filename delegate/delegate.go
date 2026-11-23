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
	// Role is what the worker is for, added to the base prompt
	// (agent.DefaultSystem), which it does not replace: a worker reads
	// what the parent would not, and keeps the base's rules for it.
	Role string
	// System, when set, is the worker's whole system prompt instead of
	// the base and Role: for a worker that is another kind of agent.
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
	Context func(ctx context.Context) ([]agent.ContextBlock, error)
	Done    func() error
}

// Team is the set of workers behind the delegate tool, and what they
// share with the parent.
type Team struct {
	Workers []Worker
	// Config is what every worker's agent starts from, normally the
	// parent's: its Provider (budget included, so delegated work counts
	// against the same limits), Approve (a worker's risky calls are
	// confirmed like the parent's), Instructions (a company's rules hold
	// for the work it delegates too), Compaction, Loop, ContextWindow,
	// AfterTool and the rest. The crew sets each worker's Tools, System
	// and Context; MaxTurns is the worker's, else this one's, else 30;
	// Provider the worker's when it has one. Observers receive every event
	// of every worker, with Event.Worker, Event.Task and Event.Parent set:
	// workers run in parallel and in the background, so observers must be
	// safe for concurrent use (see agent.Observer).
	Config agent.Config
	// MaxReport is how much of a report the parent sees; default 32 KiB.
	MaxReport int
	// MaxBackground bounds background tasks running at once: a request to
	// start another is refused until one reports, so a burst of
	// background:true cannot spawn workers and provider calls without bound.
	// Default DefaultMaxBackground.
	MaxBackground int
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
	return agent.NewTool(ToolName, c.describe(), c.call, agent.Parallel(), agent.Internal()) // a worker's own calls pass the policy
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
		b.WriteString("\n- ")
		b.WriteString(n)
		b.WriteString(": ")
		b.WriteString(c.byName[n].Description)
	}
	return b.String()
}

func (c *Crew) call(ctx context.Context, in taskIn) (string, error) {
	if strings.TrimSpace(in.Task) == "" {
		return "", agent.ToolError(agent.ErrToolInput, "The task is empty.")
	}
	if in.Background {
		return c.callBackground(ctx, in)
	}
	t, err := c.task(ctx, in)
	if err != nil {
		return "", err
	}
	defer c.release(t)
	report, err := c.work(ctx, t, in.Task)
	if err != nil {
		return "", err
	}
	return "Task " + strconv.Itoa(t.id) + " (" + t.worker.Name + "):\n" + report, nil
}

func (c *Crew) callBackground(ctx context.Context, in taskIn) (string, error) {
	if !c.reserve() {
		return "", agent.ToolError(agent.ErrToolInput, "Too many tasks are running in the background (the limit is "+strconv.Itoa(c.maxBackground())+"); wait for one to report before starting another.")
	}
	t, err := c.task(ctx, in)
	if err != nil {
		c.unreserve()
		return "", err
	}
	c.start(ctx, t, in.Task)
	return "Task " + strconv.Itoa(t.id) + " (" + t.worker.Name + ") started in the background; its report will arrive as a message. Go on with other work.", nil
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
	report, _, err := c.RunCounted(ctx, worker, taskText)
	return report, err
}

// RunCounted is Run that also reports what the task cost, failed or not.
func (c *Crew) RunCounted(ctx context.Context, worker, taskText string) (report string, spent Spend, err error) {
	t, err := c.task(ctx, taskIn{Worker: worker, Task: taskText})
	if err != nil {
		return "", spent, err
	}
	defer c.forget(t)
	defer func() { // workflow steps run on their own goroutines
		if r := recover(); r != nil {
			err = errors.New(agent.ErrToolPanic, "the worker panicked")
		}
	}()
	res, err := c.countInto(ctx, t, taskText, &spent) // spent survives a panic
	if err != nil {
		return "", spent, err
	}
	// The report alone: what it cost is in spent. A workflow passes the
	// report to the next step, whose model must not read the cost line as
	// part of the result.
	return c.body(res), spent, nil
}

// work runs one message of a task on its worker and returns the report
// with what it cost, for the model that delegated.
func (c *Crew) work(ctx context.Context, t *task, message string) (string, error) {
	var spent Spend
	res, err := c.countInto(ctx, t, message, &spent)
	if err != nil {
		return "", err
	}
	return c.body(res) + costLine(res), nil
}

// Spend is what a task cost: model calls and tokens, counted as they
// happen, so a task that fails still reports what it spent.
type Spend struct {
	Calls int         `json:"calls"`
	Usage agent.Usage `json:"usage"`
}

// Add returns the sum of s and o.
func (s Spend) Add(o Spend) Spend {
	return Spend{Calls: s.Calls + o.Calls, Usage: s.Usage.Add(o.Usage)}
}

// countInto runs one message of a task, adding what it costs to *spent
// as calls happen, so a caller that recovers a panic keeps the count.
func (c *Crew) countInto(ctx context.Context, t *task, message string, spent *Spend) (*agent.Result, error) {
	ctx = context.WithValue(ctx, infoKey{}, Info{Task: t.id, Worker: t.worker.Name})
	a, err := c.agentFor(t.worker, t.setup, t.id)
	if err != nil {
		return nil, err
	}
	var res *agent.Result
	for ev, err := range a.Stream(ctx, t.conv, agent.UserText(message)) {
		if err != nil {
			return nil, errors.Wrap(err, agent.ErrToolFailed, "the worker stopped").WithUserMessage("The worker stopped before finishing: " + agent.ModelMessage(err))
		}
		switch ev.Kind {
		case agent.EventModel:
			spent.Calls++
			spent.Usage = spent.Usage.Add(ev.Usage)
		case agent.EventCompacted: // a summary is a model call too
			spent.Usage = spent.Usage.Add(ev.Usage)
		}
		if ev.Kind == agent.EventDone {
			res = ev.Result
		}
	}
	if res == nil {
		return nil, agent.ToolError(agent.ErrToolFailed, "The worker ended without a report.")
	}
	return res, nil
}

func (c *Crew) agentFor(w Worker, setup Setup, task int) (agent.Agent, error) {
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
	cfg := c.Config
	if w.Provider != nil {
		cfg.Provider = w.Provider
	}
	cfg.Tools, cfg.Context = tools, setup.Context
	cfg.System = w.System
	if cfg.System == "" {
		cfg.System = agent.DefaultSystem + "\n\n" + w.Role
	}
	cfg.MaxTurns = firstPositive(w.MaxTurns, c.Config.MaxTurns, 30)
	cfg.Observers = tagged(c.Config.Observers, w.Name, task)
	cfg.Memory = nil // the parent's memory is the parent's: a worker recalls none
	return agent.New(cfg)
}

// tagged marks a worker's events with who ran them.
func tagged(observers []agent.Observer, worker string, task int) []agent.Observer {
	out := make([]agent.Observer, len(observers))
	for i, o := range observers {
		out[i] = func(ev agent.Event) {
			ev.Worker, ev.Task = worker, task
			o(ev)
		}
	}
	return out
}

// body is the worker's final text, bounded.
func (c *Crew) body(res *agent.Result) string {
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
	return text
}

// costLine says what a worker's turn cost.
func costLine(res *agent.Result) string {
	return "\n[worker: " + strconv.Itoa(res.Turns) + " model calls, " + strconv.Itoa(res.Usage.Input) + " in / " + strconv.Itoa(res.Usage.Output) + " out tokens; stop: " + string(res.Stop) + "]"
}

func firstPositive(vs ...int) int {
	for _, v := range vs {
		if v > 0 {
			return v
		}
	}
	return 0
}
