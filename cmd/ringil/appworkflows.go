// appworkflows.go: saved workflows, run on serve's agents
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/skills"
	"github.com/agilira/ringil-agent-sdk/trigger"
	"github.com/agilira/ringil-agent-sdk/workflow"
)

// savedDir holds the saved workflows, beside the journals of their runs.
const savedDir = "saved-workflows"

func (c *cli) savedWorkflows() (*workflow.Library, error) {
	root, err := c.stateRoot()
	if err != nil {
		return nil, err
	}
	return workflow.OpenLibrary(filepath.Join(root, savedDir))
}

// agentCrew runs a workflow's steps on serve's agents: each step is a run
// of its agent ("default", or a profile), seen in the app like any other,
// started from source (a trigger's: its runs may what the trigger was
// allowed; "" for one the user started).
type agentCrew struct {
	c      *cli
	srv    *serve.Server
	source string
}

func (a agentCrew) Has(worker string) bool {
	_, ok := a.c.conf().Profiles[worker]
	return worker == "default" || ok
}

func (a agentCrew) Workers() []string {
	return append([]string{"default"}, names(a.c.conf().Profiles)...)
}

// busyWait is how long a step waits for room on serve before it asks
// again, busyFor how long in all before it fails, saying why.
const (
	busyWait = 2 * time.Second
	busyFor  = 10 * time.Minute
)

// RunCounted runs the step as a run of its agent, waiting for room when
// serve is full (the workflow's own steps hold places).
func (a agentCrew) RunCounted(ctx context.Context, worker, task string) (string, delegate.Spend, error) {
	snap, err := a.start(ctx, worker, task)
	if err != nil {
		return "", delegate.Spend{}, err
	}
	end, err := a.srv.Wait(ctx, snap.ID)
	if err != nil {
		stop(a.srv, snap.ID)
		return "", delegate.Spend{}, err
	}
	spent := delegate.Spend{Calls: end.Turns, Usage: end.Usage}
	switch {
	case end.State == serve.Completed:
		return end.Text, spent, nil
	case end.Error != nil:
		return "", spent, errors.New(ErrUsage, "the run of "+worker+" "+string(end.State)+": "+end.Error.Message)
	}
	return "", spent, errors.New(ErrUsage, "the run of "+worker+" ended "+string(end.State))
}

// start starts a step's run, waiting while serve is full, a while.
func (a agentCrew) start(ctx context.Context, worker, task string) (serve.Snapshot, error) {
	return startWaiting(ctx, a.srv, serve.Request{Source: a.source, Profile: worker, Input: agent.UserText(task)})
}

// agentRunner is a runner of saved workflows on serve's agents, with the
// journal and budget of the settings, and the user's step hooks.
func (c *cli) agentRunner(octx *orpheus.Context, source string) (*workflow.Runner, error) {
	j, err := c.journal()
	if err != nil {
		return nil, err
	}
	budget, err := c.workflowBudget(octx)
	if err != nil {
		return nil, err
	}
	r := &workflow.Runner{Crew: agentCrew{c: c, srv: c.server, source: source}, Journal: j, Budget: budget}
	c.workflowHooks(r)
	return r, nil
}

// wfRuns are the workflows serve is running, by id, and how the ones it
// ran ended: the journal keeps the steps, this the error.
type wfRuns struct {
	mu      sync.Mutex
	running map[string]bool
	failed  map[string]string
}

func (w *wfRuns) start(id string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running == nil {
		w.running, w.failed = map[string]bool{}, map[string]string{}
	}
	w.running[id] = true
	delete(w.failed, id)
}

// claim marks id going unless it already is.
func (w *wfRuns) claim(id string) bool {
	if w == nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running[id] {
		return false
	}
	if w.running == nil {
		w.running, w.failed = map[string]bool{}, map[string]string{}
	}
	w.running[id] = true
	delete(w.failed, id)
	return true
}

func (w *wfRuns) end(id string, err error) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.running, id)
	if err != nil && w.failed != nil {
		w.failed[id] = oneLine(describe(err), 500)
	}
}

// any: a workflow is going.
func (w *wfRuns) any() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.running) > 0
}

func (w *wfRuns) state(id string) (running bool, failure string) {
	if w == nil {
		return false, ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running[id], w.failed[id]
}

// The workflows' API, mounted with the settings:
//
//	GET    /v1/workflows                       the saved workflows
//	GET    /v1/workflows/{name}                one
//	PUT    /v1/workflows/{name}                {"description","brief","plan":{"steps":[{"name","worker","skill"?,"task","needs"?}],"output"}} save one; workers are the agents
//	DELETE /v1/workflows/{name}                remove one no trigger runs
//	POST   /v1/workflows/{name}/runs           {"input"?} run it now: 202 {"id"}
//	GET    /v1/workflow-runs                   the runs the journal keeps, the latest first
//	GET    /v1/workflow-runs/{id}              one, step by step
//	POST   /v1/workflow-runs/{id}/resume       go on from where it stopped: 202
//	POST   /v1/workflow-runs/{id}/steps/{step}/retry    a step left started: run it again at the resume
//	POST   /v1/workflow-runs/{id}/steps/{step}/resolve  {"report"} a step left started: record what it did
//	DELETE /v1/workflow-runs/{id}              forget a run that is not going
func (c *cli) mountWorkflows(octx *orpheus.Context, srv *serve.Server) {
	life, cancel := context.WithCancel(context.Background())
	c.closers = append(c.closers, func() error { cancel(); return nil })
	c.workflowLife = life
	routes := map[string]http.HandlerFunc{
		"GET /v1/workflows":                                c.listSaved,
		"GET /v1/workflows/{name}":                         c.getSaved,
		"PUT /v1/workflows/{name}":                         c.putSaved,
		"DELETE /v1/workflows/{name}":                      c.deleteSaved,
		"POST /v1/workflows/{name}/runs":                   c.runSaved(octx, life),
		"GET /v1/workflow-runs":                            c.listWorkflowRuns,
		"GET /v1/workflow-runs/{id}":                       c.getWorkflowRun,
		"POST /v1/workflow-runs/{id}/resume":               c.resumeWorkflowRun(octx, life),
		"POST /v1/workflow-runs/{id}/steps/{step}/retry":   c.retryStep,
		"POST /v1/workflow-runs/{id}/steps/{step}/resolve": c.resolveStep,
		"DELETE /v1/workflow-runs/{id}":                    c.deleteWorkflowRun,
	}
	for pattern, h := range routes {
		srv.Mount(pattern, h)
	}
}

// withLibrary runs fn on the library, or says why it cannot be opened.
func (c *cli) withLibrary(w http.ResponseWriter, fn func(*workflow.Library)) {
	lib, err := c.savedWorkflows()
	if err != nil {
		fail(w, http.StatusInternalServerError, "WORKFLOW_LIBRARY", describe(err))
		return
	}
	defer closeQuietly(lib)
	fn(lib)
}

// workflowFail answers a workflow package error with its status.
func workflowFail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.HasCode(err, workflow.ErrNotFound):
		code = http.StatusNotFound
	case errors.HasCode(err, workflow.ErrPlan):
		code = http.StatusBadRequest
	case errors.HasCode(err, workflow.ErrLocked), errors.HasCode(err, workflow.ErrExists), errors.HasCode(err, workflow.ErrUncertain), errors.HasCode(err, workflow.ErrChanged):
		code = http.StatusConflict
	}
	fail(w, code, "WORKFLOW", describe(err))
}

func (c *cli) listSaved(w http.ResponseWriter, _ *http.Request) {
	c.withLibrary(w, func(lib *workflow.Library) {
		list, err := lib.List()
		if err != nil {
			workflowFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	})
}

func (c *cli) getSaved(w http.ResponseWriter, r *http.Request) {
	c.withLibrary(w, func(lib *workflow.Library) {
		s, err := lib.Get(r.PathValue("name"))
		if err != nil {
			workflowFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
}

type savedIn struct {
	Description string        `json:"description"`
	Brief       string        `json:"brief,omitempty"`
	Plan        workflow.Plan `json:"plan"`
}

func (c *cli) putSaved(w http.ResponseWriter, r *http.Request) {
	var in savedIn
	if !decodeJSONLimit(w, r, &in, 1<<20) {
		return
	}
	s := workflow.Saved{Name: r.PathValue("name"), Description: in.Description, Brief: in.Brief, Plan: in.Plan}
	if msg := c.checkSteps(s.Plan); msg != "" {
		fail(w, http.StatusBadRequest, "WORKFLOW", msg)
		return
	}
	c.withLibrary(w, func(lib *workflow.Library) {
		if err := lib.Put(s, agentCrew{c: c}.Has); err != nil {
			workflowFail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// checkSteps: every skill a step follows is installed; "" when so.
func (c *cli) checkSteps(p workflow.Plan) string {
	dir, err := c.skillsDir()
	if err != nil {
		return "no skills directory: " + describe(err)
	}
	cat, err := skills.Open(dir)
	if err != nil {
		return "the skills cannot be read: " + describe(err)
	}
	for _, s := range p.Steps {
		if s.Skill != "" && !slices.ContainsFunc(cat.List(), func(i skills.Info) bool { return i.Name == s.Skill }) {
			return "step " + s.Name + " follows the skill " + s.Skill + ", which is not installed"
		}
	}
	return ""
}

func (c *cli) deleteSaved(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if users := c.workflowUsers(name); len(users) > 0 {
		fail(w, http.StatusConflict, "WORKFLOW_IN_USE", "workflow "+name+" is run by "+joinNames(users)+": change them first")
		return
	}
	c.withLibrary(w, func(lib *workflow.Library) {
		if err := lib.Remove(name); err != nil {
			workflowFail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// workflowUsers are the triggers that run a workflow.
func (c *cli) workflowUsers(name string) []string {
	return c.triggersOf(func(t trigger.Trigger) bool { return t.Workflow == name })
}

type runSavedIn struct {
	Input string `json:"input,omitempty"`
}

// runSaved starts a saved workflow and answers with its id at once; the
// run goes on with serve, its steps seen as runs.
func (c *cli) runSaved(octx *orpheus.Context, life context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in runSavedIn
		if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
			return
		}
		lib, err := c.savedWorkflows()
		if err != nil {
			workflowFail(w, err)
			return
		}
		s, err := lib.Get(r.PathValue("name"))
		closeQuietly(lib)
		if err != nil {
			workflowFail(w, err)
			return
		}
		runner, err := c.agentRunner(octx, "")
		if err != nil {
			workflowFail(w, err)
			return
		}
		id := workflow.NewRunID(s.Name)
		c.wfRuns.start(id) // listed as going at once, before its journal exists
		go func() {
			_, err := runner.RunSaved(life, id, s, in.Input)
			c.wfRuns.end(id, err) // a run its hooks refused too
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
	}
}

// WorkflowRun is a run of a workflow as the journal keeps it.
type WorkflowRun struct {
	ID string `json:"id"`
	// State: running, done, failed (it can be resumed), or uncertain (a
	// step started and did not finish: retry or resolve it first).
	State string `json:"state"`
	Error string `json:"error,omitzero"`
	// Output is the output step's report, once done.
	Output string                         `json:"output,omitzero"`
	Plan   workflow.Plan                  `json:"plan"`
	Steps  map[string]*workflow.StepState `json:"steps"`
	Spent  delegate.Spend                 `json:"spent"`
	// Started is when its first step started.
	Started time.Time `json:"started,omitzero"`
}

func (c *cli) workflowRun(id string, st *workflow.State) WorkflowRun {
	out := WorkflowRun{ID: id, Plan: st.Plan, Steps: st.Steps, Spent: st.Spent}
	running, failure := c.wfRuns.state(id)
	pending := false
	for _, s := range st.Steps {
		pending = pending || s.Status == workflow.Pending
		if out.Started.IsZero() || s.Started.Before(out.Started) {
			out.Started = s.Started
		}
	}
	done := st.Steps[st.Plan.Output] != nil && st.Steps[st.Plan.Output].Status == workflow.Done
	switch {
	case running:
		out.State = "running"
	case done:
		out.State, out.Output = "done", st.Steps[st.Plan.Output].Report
	case pending:
		out.State = "uncertain"
	default:
		out.State = "failed"
	}
	out.Error = failure
	return out
}

func (c *cli) listWorkflowRuns(w http.ResponseWriter, _ *http.Request) {
	j, err := c.journal()
	if err != nil {
		workflowFail(w, err)
		return
	}
	ids, err := j.List()
	if err != nil {
		workflowFail(w, err)
		return
	}
	out := make([]WorkflowRun, 0, len(ids))
	for _, id := range ids {
		st, err := j.Load(context.Background(), id)
		if err != nil || st == nil {
			continue // being written, or gone
		}
		run := c.workflowRun(id, st)
		run.Plan, run.Steps = workflow.Plan{}, nil // the list: one call per run for the detail
		out = append(out, run)
	}
	slices.SortFunc(out, func(a, b WorkflowRun) int { return b.Started.Compare(a.Started) })
	writeJSON(w, http.StatusOK, out)
}

func (c *cli) getWorkflowRun(w http.ResponseWriter, r *http.Request) {
	j, err := c.journal()
	if err != nil {
		workflowFail(w, err)
		return
	}
	id := r.PathValue("id")
	st, err := j.Load(r.Context(), id)
	switch {
	case err != nil:
		workflowFail(w, err)
	case st == nil:
		if running, _ := c.wfRuns.state(id); running { // started a moment ago
			writeJSON(w, http.StatusOK, WorkflowRun{ID: id, State: "running"})
			return
		}
		fail(w, http.StatusNotFound, "WORKFLOW", "no workflow run "+id)
	default:
		writeJSON(w, http.StatusOK, c.workflowRun(id, st))
	}
}

// resumeWorkflowRun goes on with a run that stopped, on the agents.
func (c *cli) resumeWorkflowRun(octx *orpheus.Context, life context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !c.wfRuns.claim(id) {
			fail(w, http.StatusConflict, "WORKFLOW", "workflow run "+id+" is going")
			return
		}
		started := false
		defer func() {
			if !started {
				c.wfRuns.end(id, nil)
			}
		}()
		runner, err := c.agentRunner(octx, "")
		if err != nil {
			workflowFail(w, err)
			return
		}
		st, err := runner.Journal.Load(r.Context(), id)
		switch {
		case err != nil:
			workflowFail(w, err)
			return
		case st == nil:
			fail(w, http.StatusNotFound, "WORKFLOW", "no workflow run "+id)
			return
		}
		for name, s := range st.Steps {
			if s.Status == workflow.Pending {
				fail(w, http.StatusConflict, "WORKFLOW", "step "+name+" started and did not finish: retry or resolve it first")
				return
			}
		}
		started = true
		go func() {
			_, err := runner.Run(life, id, st.Plan, st.Brief, true)
			c.wfRuns.end(id, err)
		}()
		w.WriteHeader(http.StatusAccepted)
	}
}

func (c *cli) retryStep(w http.ResponseWriter, r *http.Request) {
	c.changeStep(w, r, func(j workflow.Journal, id, step string) error { return workflow.Retry(r.Context(), j, id, step) })
}

type resolveIn struct {
	Report string `json:"report"`
}

func (c *cli) resolveStep(w http.ResponseWriter, r *http.Request) {
	var in resolveIn
	if !decodeJSONLimit(w, r, &in, 1<<20) {
		return
	}
	c.changeStep(w, r, func(j workflow.Journal, id, step string) error {
		return workflow.Resolve(r.Context(), j, id, step, in.Report)
	})
}

// changeStep records what the user decided of a step left started.
func (c *cli) changeStep(w http.ResponseWriter, r *http.Request, change func(workflow.Journal, string, string) error) {
	id := r.PathValue("id")
	if running, _ := c.wfRuns.state(id); running {
		fail(w, http.StatusConflict, "WORKFLOW", "workflow run "+id+" is going")
		return
	}
	j, err := c.journal()
	if err == nil {
		err = change(j, id, r.PathValue("step"))
	}
	if err != nil {
		workflowFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *cli) deleteWorkflowRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if running, _ := c.wfRuns.state(id); running {
		fail(w, http.StatusConflict, "WORKFLOW", "workflow run "+id+" is going")
		return
	}
	j, err := c.journal()
	if err == nil {
		err = j.Remove(r.Context(), id)
	}
	if err != nil {
		workflowFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// workflowTools are the saved workflows' tools of a run of serve (their
// steps are serve's agents): running one, in any run but a trigger's;
// saving one, in a run making an automation.
func (c *cli) workflowTools() []agent.Tool {
	if c.server == nil || c.inTrigger || c.restricted() || c.serveCtx == nil || c.workflowLife == nil {
		return nil
	}
	lib, err := c.savedWorkflows()
	if err != nil {
		return nil
	}
	c.closers = append(c.closers, lib.Close)
	c.library = lib
	runner, err := c.agentRunner(c.serveCtx, "")
	if err != nil {
		return nil
	}
	tools := []agent.Tool{c.runSavedTool(runner, lib)}
	if c.automating {
		tools = append(tools, lib.SaveTool(runner.Crew, nil))
	}
	return tools
}

type runSavedToolIn struct {
	Name  string `json:"name" desc:"The saved workflow's name"`
	Input string `json:"input,omitempty" desc:"What this run is for, added to its brief as data"`
}

// runSavedTool starts a saved workflow in the background and answers its
// id at once: its steps are runs of serve, and the run that asked must
// not hold a place while they wait for one (audit 27/11: with serve full,
// the steps never started). The app shows the run; GET /v1/workflow-runs.
func (c *cli) runSavedTool(runner *workflow.Runner, lib *workflow.Library) agent.Tool {
	return agent.NewTool(workflow.RunSavedToolName, "Start a saved workflow by name. It runs in the background, its steps as runs of the agents; "+
		"the user follows it in the app. Ask the user which one when unsure.",
		func(_ context.Context, in runSavedToolIn) (string, error) {
			s, err := lib.Get(in.Name)
			if err != nil {
				return "", agent.ToolError(workflow.ErrNotFound, agent.ModelMessage(err))
			}
			id := workflow.NewRunID(s.Name)
			c.wfRuns.start(id)
			go func() {
				_, err := runner.RunSaved(c.workflowLife, id, s, in.Input)
				c.wfRuns.end(id, err)
			}()
			return "Workflow " + s.Name + " started as " + id + ": it runs in the background, the user sees its steps and its result in the app.", nil
		}, agent.Destructive())
}

// savedList names the saved workflows for the model, when it may run them.
func (c *cli) savedList() string {
	if c.library == nil {
		return ""
	}
	list, err := c.library.List()
	if err != nil || len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<saved_workflows>\nWorkflows the user saved; run one with run_saved_workflow when the user asks for it:\n")
	for _, s := range list {
		b.WriteString("- ")
		b.WriteString(s.Name)
		b.WriteString(": ")
		b.WriteString(oneLine(s.Description, 200))
		b.WriteString(" (")
		b.WriteString(strconv.Itoa(len(s.Plan.Steps)))
		b.WriteString(" steps)\n")
	}
	b.WriteString("</saved_workflows>")
	return b.String()
}
