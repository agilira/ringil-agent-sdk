// crew.go: the tasks of a team, kept to be continued or run in background
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package delegate

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"sync"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Crew runs a team's tasks. A finished task keeps its worker's
// conversation, so the parent can send it a follow-up; a background task
// reports through Next. Close releases everything.
type Crew struct {
	Team
	byName map[string]Worker
	names  []string

	life   context.Context // background tasks end with the crew
	cancel context.CancelFunc
	bg     sync.WaitGroup // background tasks, awaited by Close

	mu       sync.Mutex
	closed   bool
	next     int
	tasks    map[int]*task
	order    []int // oldest first, for dropping the oldest finished task
	finished []Finished
	running  int // background tasks still running
	ready    chan struct{}
}

type task struct {
	id     int
	worker Worker
	setup  Setup
	conv   agent.Conversation
	busy   bool
}

// Finished is the outcome of a background task.
type Finished struct {
	Task   int
	Worker string
	Report string
	Err    error
}

// keep bounds the finished tasks held for follow-ups; the oldest go first.
const keep = 32

// NewCrew checks the team and prepares its crew.
func NewCrew(t Team) (*Crew, error) {
	c := &Crew{Team: t, byName: map[string]Worker{}, tasks: map[int]*task{}, ready: make(chan struct{}, 1)}
	for _, w := range t.Workers {
		if !validName(w.Name) || c.byName[w.Name].Name != "" {
			return nil, errors.New(ErrConfig, "worker names are distinct lowercase words")
		}
		if w.Provider == nil && t.Provider == nil {
			return nil, errors.New(ErrConfig, "worker "+w.Name+" has no model")
		}
		c.byName[w.Name] = w
		c.names = append(c.names, w.Name)
	}
	if len(c.names) == 0 {
		return nil, errors.New(ErrConfig, "a team needs at least one worker")
	}
	sort.Strings(c.names)
	c.life, c.cancel = context.WithCancel(context.Background())
	return c, nil
}

// add registers a new task, busy, dropping the oldest idle one beyond
// the bound.
func (c *Crew) add(w Worker, setup Setup) *task {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	t := &task{id: c.next, worker: w, setup: setup, conv: agent.NewConversation(), busy: true}
	c.tasks[t.id] = t
	c.order = append(c.order, t.id)
	c.trim()
	return t
}

func (c *Crew) trim() {
	for len(c.tasks) > keep {
		oldest := c.oldestIdle()
		if oldest == nil {
			return // all busy: trimmed when one finishes
		}
		c.drop(oldest)
	}
}

func (c *Crew) oldestIdle() *task {
	for _, id := range c.order {
		if t := c.tasks[id]; t != nil && !t.busy {
			return t
		}
	}
	return nil
}

// resume marks a finished task busy again for a follow-up.
func (c *Crew) resume(id int) (*task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tasks[id]
	switch {
	case !ok:
		return nil, agent.ToolError(agent.ErrToolInput, "No task "+strconv.Itoa(id)+" to continue; it may be too old. Start a new one.")
	case t.busy:
		return nil, agent.ToolError(agent.ErrToolInput, "Task "+strconv.Itoa(id)+" is still running; wait for its report.")
	}
	t.busy = true
	return t, nil
}

// release marks a task idle, ready for a follow-up; after Close it is
// dropped instead.
func (c *Crew) release(t *task) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t.busy = false
	if c.closed {
		c.drop(t)
		return
	}
	c.touch(t.id) // the order is by last use: a task just finished is the newest
	c.trim()
}

func (c *Crew) touch(id int) {
	for i, o := range c.order {
		if o == id {
			c.order = append(append(c.order[:i:i], c.order[i+1:]...), id)
			return
		}
	}
}

// drop forgets a task and releases its resources.
func (c *Crew) drop(t *task) {
	delete(c.tasks, t.id)
	for i, id := range c.order {
		if id == t.id {
			c.order = append(c.order[:i:i], c.order[i+1:]...)
			break
		}
	}
	cleanup(t.setup)
}

// forget drops a task that will not be continued, such as a workflow
// step's.
func (c *Crew) forget(t *task) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop(t)
}

// start runs a task in the background. It outlives the tool call that
// started it, not the crew.
func (c *Crew) start(ctx context.Context, t *task, message string) {
	c.mu.Lock()
	c.running++
	c.mu.Unlock()
	bg := context.WithoutCancel(ctx)
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		run, stop := context.WithCancel(bg)
		defer stop()
		unhook := context.AfterFunc(c.life, stop)
		defer unhook()
		report, err := c.workSafely(run, t, message)
		c.release(t)
		c.finish(Finished{Task: t.id, Worker: t.worker.Name, Report: report, Err: err})
	}()
}

// workSafely turns a panic in a background task into its error: there is
// no registry around it to catch one.
func (c *Crew) workSafely(ctx context.Context, t *task, message string) (report string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New(agent.ErrToolPanic, "the worker panicked")
		}
	}()
	return c.work(ctx, t, message)
}

func (c *Crew) finish(f Finished) {
	c.mu.Lock()
	c.running--
	c.finished = append(c.finished, f)
	c.mu.Unlock()
	select {
	case c.ready <- struct{}{}:
	default:
	}
}

// Pending reports background tasks running or finished but not yet taken
// with Next.
func (c *Crew) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running + len(c.finished)
}

// Next waits for the next finished background task. It returns false
// when none is running or waiting, or when ctx ends.
func (c *Crew) Next(ctx context.Context) (Finished, bool) {
	for {
		c.mu.Lock()
		if len(c.finished) > 0 {
			f := c.finished[0]
			c.finished = c.finished[1:]
			c.mu.Unlock()
			return f, true
		}
		running := c.running
		c.mu.Unlock()
		if running == 0 {
			return Finished{}, false
		}
		select {
		case <-c.ready:
		case <-ctx.Done():
			return Finished{}, false
		}
	}
}

// Notice is the message that brings a background report to the parent:
// marked as a task's report, not as words of the user.
func Notice(f Finished) agent.Message {
	body := f.Report
	if f.Err != nil {
		body = "The task failed: " + agent.ModelMessage(f.Err)
	}
	return agent.UserText("<task-report task=\"" + strconv.Itoa(f.Task) + "\" worker=\"" + f.Worker + "\">\n" +
		"This is the report of background task " + strconv.Itoa(f.Task) + ", not a message from the user. Use it to go on with your work.\n" +
		closeTag.ReplaceAllString(body, "</task_report>") + "\n</task-report>")
}

var closeTag = regexp.MustCompile(`(?i)<\s*/\s*task-report\s*>`)

// Close stops background tasks, waits for them to stop, and releases
// every task's resources. A synchronous task still running (in a tool
// call) releases its own when it returns.
func (c *Crew) Close() {
	c.cancel()
	c.mu.Lock()
	c.closed = true
	for _, t := range c.tasks {
		if !t.busy {
			c.drop(t)
		}
	}
	c.mu.Unlock()
	c.bg.Wait()
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

// Has reports whether the crew has a worker of that name.
func (c *Crew) Has(name string) bool {
	_, ok := c.byName[name]
	return ok
}

// Workers lists the crew's worker names.
func (c *Crew) Workers() []string { return append([]string(nil), c.names...) }

type workerIn struct {
	Description  string `json:"description,omitempty" desc:"A few words naming the task, shown to the user"`
	Task         string `json:"task" desc:"The complete task: the worker sees nothing else, so include the goal, what you know, where to look and what to report back. To continue a task, your follow-up message"`
	Background   bool   `json:"background,omitempty" desc:"Start it and go on working; its report arrives later as a message"`
	ContinueTask int    `json:"continue_task,omitempty" min:"1" desc:"Number of a finished task of this worker to send a follow-up to"`
}

// WorkerTools returns one tool per worker, named after it: models tend
// to call a worker by its name, and a provider may drop a call to a name
// it was not given. Each does what delegate does for that worker.
func (c *Crew) WorkerTools() []agent.Tool {
	tools := make([]agent.Tool, 0, len(c.names))
	for _, n := range c.names {
		w := c.byName[n]
		desc := w.Description + ". A sub-agent with a clean context: give it a complete task; it returns one report, numbered. Use background to keep working meanwhile, continue_task to ask it more."
		tools = append(tools, agent.NewTool(n, desc, func(ctx context.Context, in workerIn) (string, error) {
			return c.call(ctx, taskIn{Worker: n, Description: in.Description, Task: in.Task, Background: in.Background, ContinueTask: in.ContinueTask})
		}, agent.Parallel()))
	}
	return tools
}
