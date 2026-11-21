// jobs.go: commands that run in the background
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"context"
	"strconv"
	"sync"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Bounds of the background jobs.
const (
	// DefaultBackgroundTimeout is how long a background command may run
	// when Config.BackgroundTimeout does not say: then it is stopped, so
	// that none outlives what it was for.
	DefaultBackgroundTimeout = time.Hour
	// maxRunning is how many background commands run at once.
	maxRunning = 8
	// maxKept is how many jobs, running or ended, the tool remembers.
	maxKept = 32
)

// job is a command running, or run, in the background.
type job struct {
	id      string
	command string
	out     *capture
	started time.Time
	stop    context.CancelFunc
	done    chan struct{}
	status  int // when done
	result  agent.ToolOutput
}

type jobs struct {
	mu     sync.Mutex
	byID   map[string]*job
	order  []string
	n      int
	closed bool
}

func (t *tool) isClosed() bool {
	t.jobs.mu.Lock()
	defer t.jobs.mu.Unlock()
	return t.jobs.closed
}

// close stops every job and waits for each, its repository check
// included.
func (t *tool) close() error {
	t.jobs.mu.Lock()
	t.jobs.closed = true
	all := make([]*job, 0, len(t.jobs.byID))
	for _, j := range t.jobs.byID {
		all = append(all, j)
	}
	t.jobs.mu.Unlock()
	for _, j := range all {
		j.stop()
	}
	for _, j := range all {
		<-j.done
	}
	return nil
}

// start runs command in the background with run, as call would in the
// foreground, and returns its job.
func (t *tool) start(command string, outside bool) (*job, error) {
	t.jobs.mu.Lock()
	defer t.jobs.mu.Unlock()
	if t.jobs.closed {
		return nil, agent.ToolError(ErrRun, "The shell is closed.")
	}
	if t.jobs.running() >= maxRunning {
		return nil, agent.ToolError(ErrRun, "There are "+strconv.Itoa(maxRunning)+" background commands running already: stop one with bash_kill first.")
	}
	timeout := t.cfg.BackgroundTimeout
	if timeout <= 0 {
		timeout = DefaultBackgroundTimeout
	}
	// The call that starts it ends now: the job has a context of its own.
	ctx, stop := context.WithTimeout(context.Background(), timeout)
	t.jobs.n++
	j := &job{id: "b" + strconv.Itoa(t.jobs.n), command: command, out: &capture{max: t.cfg.MaxOutput},
		started: time.Now(), stop: stop, done: make(chan struct{})}
	run := t.runSandboxed
	if outside {
		run = t.runHost
	}
	go func() {
		defer close(j.done)
		defer stop()
		err := run(ctx, command, j.out)
		res, failed := t.result(ctx, j.out, err, timeout)
		if failed != nil {
			res = agent.ToolOutput{Content: failed.Error()} // it did not start: say why (audit 21/11)
		}
		j.result = res
		j.status, _ = exitStatus(err)
	}()
	t.jobs.keep(j)
	return j, nil
}

func (js *jobs) running() int {
	n := 0
	for _, j := range js.byID {
		select {
		case <-j.done:
		default:
			n++
		}
	}
	return n
}

// keep remembers j, forgetting the oldest ended job past maxKept.
func (js *jobs) keep(j *job) {
	if js.byID == nil {
		js.byID = map[string]*job{}
	}
	js.byID[j.id] = j
	js.order = append(js.order, j.id)
	for len(js.order) > maxKept {
		oldest := js.byID[js.order[0]]
		select {
		case <-oldest.done:
			delete(js.byID, oldest.id)
			js.order = js.order[1:]
		default:
			return // all running: kept until they end
		}
	}
}

func (js *jobs) get(id string) (*job, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	j, ok := js.byID[id]
	return j, ok
}

type jobIn struct {
	ID string `json:"id" desc:"The job's ID, as bash gave it (b1, b2...)"`
}

func (t *tool) outputTool() agent.Tool {
	return agent.NewTool("bash_output", "Read what a background bash command has printed so far, and whether it is still running.", t.output, agent.ReadOnly())
}

func (t *tool) killTool() agent.Tool {
	return agent.NewTool("bash_kill", "Stop a background bash command.", t.kill) // only what bash started: no one need approve it
}

func (t *tool) output(_ context.Context, in jobIn) (string, error) {
	j, ok := t.jobs.get(in.ID)
	if !ok {
		return "", agent.ToolError(ErrInput, "No background command "+strconv.Quote(in.ID)+".")
	}
	select {
	case <-j.done:
		return "Ended after " + j.since() + ".\n" + j.result.Content, nil
	default:
		text := j.out.String()
		if text == "" {
			text = "(no output yet)"
		}
		return "Running for " + j.since() + ".\n" + text, nil
	}
}

func (t *tool) kill(_ context.Context, in jobIn) (string, error) {
	j, ok := t.jobs.get(in.ID)
	if !ok {
		return "", agent.ToolError(ErrInput, "No background command "+strconv.Quote(in.ID)+".")
	}
	j.stop()
	select {
	case <-j.done:
	case <-time.After(5 * time.Second):
		return "Asked to stop; it has not ended yet.", nil
	}
	return "Stopped after " + j.since() + ".\n" + j.result.Content, nil
}

func (j *job) since() string { return time.Since(j.started).Round(time.Second).String() }
