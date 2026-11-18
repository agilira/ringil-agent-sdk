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
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

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
	Worker      string `json:"worker" desc:"Which kind of worker"`
	Description string `json:"description,omitempty" desc:"A few words naming the task, shown to the user"`
	Task        string `json:"task" desc:"The complete task: the worker sees nothing else, so include the goal, what you know, where to look and what to report back"`
}

type team struct {
	Team
	byName map[string]Worker
	next   atomic.Int64
}

// Tool returns the delegate tool for t.
func Tool(t Team) (agent.Tool, error) {
	tm := &team{Team: t, byName: map[string]Worker{}}
	var names []string
	for _, w := range t.Workers {
		if !validName(w.Name) || tm.byName[w.Name].Name != "" {
			return nil, errors.New(ErrConfig, "worker names are distinct lowercase words")
		}
		if w.Provider == nil && t.Provider == nil {
			return nil, errors.New(ErrConfig, "worker "+w.Name+" has no model")
		}
		tm.byName[w.Name] = w
		names = append(names, w.Name)
	}
	if len(names) == 0 {
		return nil, errors.New(ErrConfig, "a team needs at least one worker")
	}
	sort.Strings(names)
	return agent.NewTool(ToolName, tm.describe(names), tm.run, agent.Parallel()), nil
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

func (tm *team) describe(names []string) string {
	var b strings.Builder
	b.WriteString("Hand a self-contained task to a sub-agent with a clean context; it returns one final report. Use it for searches and work that would fill your context, and call it several times in one turn to run tasks in parallel. Write the task in full: the worker knows nothing of this conversation. Workers:")
	for _, n := range names {
		b.WriteString("\n- " + n + ": " + tm.byName[n].Description)
	}
	return b.String()
}

func (tm *team) run(ctx context.Context, in taskIn) (string, error) {
	w, ok := tm.byName[in.Worker]
	if !ok {
		return "", agent.ToolError(agent.ErrToolInput, "No worker named "+strconv.Quote(in.Worker)+".")
	}
	if strings.TrimSpace(in.Task) == "" {
		return "", agent.ToolError(agent.ErrToolInput, "The task is empty.")
	}
	id := int(tm.next.Add(1))
	setup, err := prepare(ctx, w, in.Task)
	if err != nil {
		return "", err
	}
	defer cleanup(setup) // also after a panic, caught by the parent's registry
	ctx = context.WithValue(ctx, infoKey{}, Info{Task: id, Worker: w.Name})
	return tm.work(ctx, id, w, setup, in.Task)
}

func cleanup(s Setup) {
	if s.Done == nil {
		return
	}
	if err := s.Done(); err != nil {
		return // temporary state; the report already stands
	}
}

// Info identifies the worker a call comes from.
type Info struct {
	Task   int
	Worker string
}

type infoKey struct{}

// Current reports the worker running on ctx: hooks shared with the parent
// (approval, access requests) use it to say who is asking.
func Current(ctx context.Context) (Info, bool) {
	i, ok := ctx.Value(infoKey{}).(Info)
	return i, ok
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

// work runs one task on a fresh agent and returns its report.
func (tm *team) work(ctx context.Context, id int, w Worker, setup Setup, task string) (string, error) {
	a, err := tm.agentFor(w, setup)
	if err != nil {
		return "", err
	}
	var res *agent.Result
	for ev, err := range a.Stream(ctx, agent.NewConversation(), agent.UserText(task)) {
		if err != nil {
			return "", errors.Wrap(err, agent.ErrToolFailed, "the worker stopped").WithUserMessage("The worker stopped before finishing: " + agent.ModelMessage(err))
		}
		if tm.OnEvent != nil {
			tm.OnEvent(id, w.Name, ev)
		}
		if ev.Kind == agent.EventDone {
			res = ev.Result
		}
	}
	if res == nil {
		return "", agent.ToolError(agent.ErrToolFailed, "The worker ended without a report.")
	}
	return tm.report(res), nil
}

func (tm *team) agentFor(w Worker, setup Setup) (agent.Agent, error) {
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
		p = tm.Provider
	}
	return agent.New(agent.Config{
		Provider: p, Tools: tools, System: w.System, Context: setup.Context,
		Approve: tm.Approve, MaxTurns: firstPositive(w.MaxTurns, tm.MaxTurns, 30),
	})
}

// report is the worker's final text, bounded, with what it cost.
func (tm *team) report(res *agent.Result) string {
	text := strings.TrimSpace(res.Text)
	if text == "" {
		text = "(the worker gave no final text)"
	}
	limit := firstPositive(tm.MaxReport, 32<<10)
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
