// workflow.go: explicit plans of steps run by workers
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package workflow runs a plan: named steps, each a task for a worker of a
// Crew (a delegate.Crew, or the host's own agents), with the steps it
// needs. A step starts when its needs
// are done, steps that are ready run at the same time, and each sees only
// its task, the brief and the reports of the steps it needs.
//
// With a Journal the run survives an interruption: the intent to run a
// step is saved before the step starts and its report before the plan
// moves on. Resuming skips the steps done; a step found started but not
// finished stops the resume (ErrUncertain), since its effects may have
// happened: the operator checks and records its outcome with Resolve.
// Nothing is ever run twice by the journal's choice.
package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/dirs"
	"github.com/agilira/ringil-agent-sdk/skills"
)

// Error codes.
const (
	ErrPlan      errors.ErrorCode = "WORKFLOW_PLAN"
	ErrExists    errors.ErrorCode = "WORKFLOW_EXISTS"
	ErrChanged   errors.ErrorCode = "WORKFLOW_CHANGED"
	ErrUncertain errors.ErrorCode = "WORKFLOW_UNCERTAIN"
	ErrStep      errors.ErrorCode = "WORKFLOW_STEP"
	ErrJournal   errors.ErrorCode = "WORKFLOW_JOURNAL"
	ErrBudget    errors.ErrorCode = "WORKFLOW_BUDGET"
)

// Step is one task of a plan.
type Step struct {
	Name   string `json:"name"`
	Worker string `json:"worker"`
	// Skill, when set, is a skill the worker follows for the task: it is
	// told to load it first (a worker with the load_skill tool).
	Skill string   `json:"skill,omitempty"`
	Task  string   `json:"task"`
	Needs []string `json:"needs,omitempty"`
}

// Plan is a set of steps and the step whose report is the result.
type Plan struct {
	Steps  []Step `json:"steps"`
	Output string `json:"output"`
}

var stepName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Check validates the plan: distinct names, known workers, needs that
// exist, no cycle, an output step.
func (p Plan) Check(knownWorker func(string) bool) error {
	byName := map[string]Step{}
	for _, s := range p.Steps {
		if byName[s.Name].Name != "" {
			return errors.New(ErrPlan, "two steps are named "+s.Name)
		}
		if err := s.check(knownWorker); err != nil {
			return err
		}
		byName[s.Name] = s
	}
	if len(byName) == 0 || byName[p.Output].Name == "" {
		return errors.New(ErrPlan, "a plan needs steps and an output step among them")
	}
	return acyclic(byName)
}

// check validates one step: its name, worker, task and skill.
func (s Step) check(knownWorker func(string) bool) error {
	switch {
	case !stepName.MatchString(s.Name):
		return errors.New(ErrPlan, "step names are lowercase words: "+s.Name)
	case knownWorker != nil && !knownWorker(s.Worker):
		return errors.New(ErrPlan, "step "+s.Name+" names an unknown worker "+s.Worker)
	case strings.TrimSpace(s.Task) == "":
		return errors.New(ErrPlan, "step "+s.Name+" has no task")
	case s.Skill != "" && !skills.ValidName(s.Skill):
		return errors.New(ErrPlan, "step "+s.Name+" names a skill that is not a skill's name: "+s.Skill)
	}
	return nil
}

// acyclic checks needs exist and never loop, by repeatedly removing steps
// whose needs are all removed.
func acyclic(steps map[string]Step) error {
	for _, s := range steps {
		for _, n := range s.Needs {
			if steps[n].Name == "" {
				return errors.New(ErrPlan, "step "+s.Name+" needs an unknown step "+n)
			}
		}
	}
	done := map[string]bool{}
	for progress := true; progress; {
		progress = false
		for name, s := range steps {
			if !done[name] && all(s.Needs, done) {
				done[name], progress = true, true
			}
		}
	}
	if len(done) != len(steps) {
		return errors.New(ErrPlan, "the steps' needs form a cycle")
	}
	return nil
}

func all(names []string, set map[string]bool) bool {
	for _, n := range names {
		if !set[n] {
			return false
		}
	}
	return true
}

// Fingerprint identifies a plan and its brief: a resumed run must be the
// same work.
func (p Plan) Fingerprint(brief string) string {
	data, err := json.Marshal(struct {
		Plan  Plan
		Brief string
	}{p, brief})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:12])
}

// Step statuses in the journal.
const (
	Pending = "pending" // started; its outcome is not known
	Done    = "done"
)

// State is what the journal keeps of one run.
type State struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
	// Plan and Brief are kept so that a run can be resumed from the
	// journal alone; the journal is as private as the agent's sessions.
	Plan  Plan                  `json:"plan"`
	Brief string                `json:"brief,omitempty"`
	Steps map[string]*StepState `json:"steps"`
	// Spent is what the run cost so far, over every attempt: a resume
	// goes on counting.
	Spent delegate.Spend `json:"spent"`
}

// StepState is one step's record.
type StepState struct {
	Status   string         `json:"status"`
	Report   string         `json:"report,omitempty"`
	Resolved bool           `json:"resolved,omitempty"` // recorded by the operator
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished,omitzero"`
	Spent    delegate.Spend `json:"spent"`
}

// Journal keeps runs' states durably. Load returns nil when the run is
// unknown.
type Journal interface {
	Load(ctx context.Context, id string) (*State, error)
	Save(ctx context.Context, id string, s *State) error
}

// Locker is implemented by journals that hold a run exclusively.
type Locker interface {
	Lock(ctx context.Context, id string) (unlock func() error, err error)
}

// Crew is who does the steps: a *delegate.Crew, or the host's own (its
// agents, each a worker by name).
type Crew interface {
	// Has says whether worker is one of the crew.
	Has(worker string) bool
	// Workers names them, for the model writing a plan.
	Workers() []string
	// RunCounted runs one task on worker and returns its report and what
	// it spent.
	RunCounted(ctx context.Context, worker, task string) (report string, spent delegate.Spend, err error)
}

// Runner runs plans on a crew.
type Runner struct {
	Crew Crew
	// Journal makes runs resumable. Nil is a FileJournal in the program's
	// state (dirs.Program().State, "workflows"), made at the first run;
	// NoJournal keeps runs in memory, not resumable after a restart.
	Journal   Journal
	NoJournal bool
	// Parallel bounds the steps running at once; default 4.
	Parallel int
	// OnStep reports progress: a step started, done or failed.
	OnStep func(step, status string, err error)
	// BeforeRun, when set, is asked before a run (or a resume) starts: an
	// error refuses it, before the journal is touched; a panic too.
	BeforeRun func(ctx context.Context, id string, plan Plan) error
	// AfterRun, when set, is told how each run BeforeRun let start ended:
	// its result, or its error. A panic in it is ignored.
	AfterRun func(ctx context.Context, id string, res *Result, err error)
	// BeforeStep, when set, is asked before each step starts, with the
	// run's id: it returns the step to run (as is, or with another Task or
	// Skill; its name, worker and needs stay the plan's), or an error that
	// stops the run before the step starts (the step is not marked: a
	// resume runs it). A panic stops it too.
	BeforeStep func(ctx context.Context, id string, step Step) (Step, error)
	// AfterStep, when set, is told how each step that ran ended: its
	// report, or its error. It is called after the journal holds the
	// outcome; a panic in it is ignored.
	AfterStep func(ctx context.Context, id string, step Step, report string, err error)
	// Budget bounds what a run may spend in total, across resumes and
	// retries; zero fields are unbounded. A step starts only while the
	// run is within it, and spending is counted when a step ends: the
	// steps already running may go past it by their own cost.
	Budget Budget

	once sync.Once
}

// defaultJournal gives the runner its journal, once, when the host set
// none and did not ask for none. A journal that cannot be made leaves the
// runs in memory: they still run.
func (r *Runner) defaultJournal() {
	r.once.Do(func() {
		if r.Journal != nil || r.NoJournal {
			return
		}
		d, err := dirs.Program()
		if err != nil {
			return
		}
		if j, err := NewFileJournal(filepath.Join(d.State, "workflows")); err == nil {
			r.Journal = j
		}
	})
}

// Budget limits a run.
type Budget struct {
	Calls  int // model calls that succeeded (retries inside a provider are not counted)
	Tokens int // input plus output tokens
	// Cost is in US dollars, from Usage.Cost: the workers' providers are
	// priced by the price package.
	Cost float64
}

func (b Budget) exceeded(s delegate.Spend) bool {
	return (b.Calls > 0 && s.Calls >= b.Calls) || (b.Tokens > 0 && s.Usage.Input+s.Usage.Output >= b.Tokens) ||
		(b.Cost > 0 && s.Usage.Cost >= b.Cost)
}

// Result is a finished run.
type Result struct {
	Output  string
	Reports map[string]string
}

// Run runs plan with id; resume continues a run the journal holds.
func (r *Runner) Run(ctx context.Context, id string, plan Plan, brief string, resume bool) (*Result, error) {
	if err := r.beforeRun(ctx, id, plan); err != nil {
		return nil, err
	}
	res, err := r.run(ctx, id, plan, brief, resume)
	r.afterRun(ctx, id, res, err)
	return res, err
}

// beforeRun asks Runner.BeforeRun; a panic refuses the run.
func (r *Runner) beforeRun(ctx context.Context, id string, plan Plan) (err error) {
	if r.BeforeRun == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			err = errors.New(ErrStep, "the BeforeRun hook failed before workflow "+id)
		}
	}()
	if err := r.BeforeRun(ctx, id, plan); err != nil {
		return errors.Wrap(err, ErrStep, "the BeforeRun hook refused workflow "+id)
	}
	return nil
}

// afterRun tells Runner.AfterRun; a panic in it is ignored.
func (r *Runner) afterRun(ctx context.Context, id string, res *Result, err error) {
	if r.AfterRun == nil {
		return
	}
	defer func() { _ = recover() }()
	r.AfterRun(context.WithoutCancel(ctx), id, res, err)
}

func (r *Runner) run(ctx context.Context, id string, plan Plan, brief string, resume bool) (*Result, error) {
	r.defaultJournal()
	if noCrew(r.Crew) {
		return nil, errors.New(ErrPlan, "the runner has no crew")
	}
	if err := plan.Check(r.Crew.Has); err != nil {
		return nil, err
	}
	unlock, err := r.lock(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release(unlock)
	st, err := r.begin(ctx, id, plan, brief, resume)
	if err != nil {
		return nil, err
	}
	e := &execution{r: r, id: id, plan: plan, brief: brief, st: st}
	if err := e.run(ctx); err != nil {
		return nil, err
	}
	return e.result(), nil
}

// noCrew: none, or a nil *delegate.Crew in the interface.
func noCrew(c Crew) bool {
	dc, ok := c.(*delegate.Crew)
	return c == nil || (ok && dc == nil)
}

func (r *Runner) lock(ctx context.Context, id string) (func() error, error) {
	if l, ok := r.Journal.(Locker); ok {
		return l.Lock(ctx, id)
	}
	return nil, nil
}

func release(unlock func() error) {
	if unlock == nil {
		return
	}
	if err := unlock(); err != nil {
		return // the lock goes with the process
	}
}

// begin loads or starts the run's state and checks a resume is safe.
func (r *Runner) begin(ctx context.Context, id string, plan Plan, brief string, resume bool) (*State, error) {
	fresh := &State{Version: 1, Fingerprint: plan.Fingerprint(brief), Plan: plan, Brief: brief, Steps: map[string]*StepState{}}
	if r.Journal == nil {
		return fresh, nil
	}
	st, err := r.Journal.Load(ctx, id)
	switch {
	case err != nil:
		return nil, errors.Wrap(err, ErrJournal, "cannot read the workflow journal")
	case st == nil && resume:
		return nil, errors.New(ErrJournal, "no workflow "+id+" to resume")
	case st == nil:
		return fresh, r.Journal.Save(ctx, id, fresh)
	case !resume:
		return nil, errors.New(ErrExists, "workflow "+id+" already exists; resume it or use another id")
	case st.Fingerprint != fresh.Fingerprint:
		return nil, errors.New(ErrChanged, "workflow "+id+" was started with another plan or brief")
	}
	if p := pendingSteps(st); len(p) > 0 {
		return nil, errors.New(ErrUncertain, "steps "+strings.Join(p, ", ")+" started and did not finish; check their effects, then record their outcome with Resolve")
	}
	return st, nil
}

func pendingSteps(st *State) []string {
	var out []string
	for name, s := range st.Steps {
		if s.Status == Pending {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Resolve records the verified outcome of a step left pending, so that a
// resume goes on without running it again.
func Resolve(ctx context.Context, j Journal, id, step, report string) error {
	return change(ctx, j, id, step, func(s *StepState) {
		s.Status, s.Report, s.Resolved, s.Finished = Done, report, true, time.Now()
	})
}

// Retry clears a step left pending, so that a resume runs it again: for
// the operator who checked that running it twice is safe (it failed
// before doing anything, or its effects are idempotent).
func Retry(ctx context.Context, j Journal, id, step string) error {
	return change(ctx, j, id, step, nil)
}

// change edits a pending step while holding the run, so that a live run
// and the operator never overwrite each other.
func change(ctx context.Context, j Journal, id, step string, fn func(*StepState)) error {
	if l, ok := j.(Locker); ok {
		unlock, err := l.Lock(ctx, id)
		if err != nil {
			return err
		}
		defer release(unlock)
	}
	st, err := j.Load(ctx, id)
	switch {
	case err != nil:
		return errors.Wrap(err, ErrJournal, "cannot read the journal of "+id)
	case st == nil:
		return errors.New(ErrNotFound, "no workflow run "+id)
	}
	s := st.Steps[step]
	if s == nil || s.Status != Pending {
		return errors.New(ErrNotFound, "no step "+step+" left started in "+id)
	}
	if fn == nil {
		delete(st.Steps, step)
	} else {
		fn(s)
	}
	return j.Save(ctx, id, st)
}

// execution is one run in progress.
type execution struct {
	r     *Runner
	id    string
	plan  Plan
	brief string
	mu    sync.Mutex
	st    *State
}
