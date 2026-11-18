// schedule.go: running jobs on their timetable
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package schedule runs agents on a timetable: "every weekday at 8",
// "every 10 minutes". Jobs live in a Store (a private directory: jobs.json
// people edit, status.json the scheduler writes); a Scheduler reads them,
// starts each run through Config.Run when it is due, and records what
// became of it.
//
// The rules a scheduler keeps:
//   - one run of a job at a time: a run due while the last is still going
//     is skipped and counted;
//   - no burst after downtime: runs missed while the scheduler was down,
//     or the machine asleep, are skipped, except one catch-up run for jobs
//     that ask for it;
//   - one scheduler per store: a second one refuses to start;
//   - edits to the jobs are picked up while running (Reload, or within
//     Config.Poll).
//
// A scheduled run has no one in front of it: whatever its tools would ask
// waits for an answer through the host (serve's API) and is refused when
// none comes. The CLI runs jobs through `ringil serve`.
package schedule

import (
	"context"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

// Run starts a job's run and waits for its end. It returns what the run
// produced and its error, nil for success.
type Run func(ctx context.Context, job Job) (Outcome, error)

// Outcome is what a run produced.
type Outcome struct {
	// Run is the run's ID with the host (serve keeps runs for a while).
	Run string
	// Text is the agent's answer; the status keeps its first MaxText bytes.
	Text string
}

// MaxText bounds the answer a job's status keeps.
const MaxText = 4 << 10

// EventKind is what happened to a job.
type EventKind string

const (
	JobStarted  EventKind = "started"
	JobFinished EventKind = "finished"
	JobFailed   EventKind = "failed"
	JobSkipped  EventKind = "skipped"
	// JobInvalid: the job's timetable or zone cannot be read; it does not
	// run until fixed.
	JobInvalid EventKind = "invalid"
)

// Event reports a job's run to Config.OnEvent.
type Event struct {
	Kind EventKind
	Job  string
	// At is the scheduled time of the run.
	At  time.Time
	Run string
	Err error
}

// Clock is time for the scheduler; tests replace it.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Config configures a Scheduler.
type Config struct {
	Store *Store
	Run   Run
	// OnEvent hears of every start, end and skip; it must not block.
	OnEvent func(Event)
	// Poll is how often the jobs are re-read; default 30s.
	Poll  time.Duration
	Clock Clock
}

// Scheduler runs a store's jobs.
type Scheduler struct {
	cfg    Config
	reload chan struct{}

	mu      sync.Mutex
	status  map[string]Status
	running map[string]bool
	jobs    map[string]planned
}

// planned is a job as the scheduler holds it.
type planned struct {
	job  Job
	when When
	next time.Time
}

// New checks the configuration.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Store == nil || cfg.Run == nil {
		return nil, errors.New(ErrConfig, "a scheduler needs a store and a way to run jobs")
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 30 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.OnEvent == nil {
		cfg.OnEvent = func(Event) {}
	}
	return &Scheduler{cfg: cfg, reload: make(chan struct{}, 1), running: map[string]bool{}, jobs: map[string]planned{}}, nil
}

// Reload re-reads the jobs now instead of at the next poll.
func (s *Scheduler) Reload() {
	select {
	case s.reload <- struct{}{}:
	default:
	}
}

// Run runs the jobs until ctx ends, then waits for the runs in progress
// (which end with ctx). It returns ErrBusy when another scheduler holds
// the store.
func (s *Scheduler) Run(ctx context.Context) error {
	release, err := s.cfg.Store.claim()
	if err != nil {
		return err
	}
	defer release()
	if s.status, err = s.cfg.Store.Status(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	s.load(true)
	loaded := s.cfg.Clock.Now()
	for {
		now := s.cfg.Clock.Now()
		for _, due := range s.due(now) {
			wg.Go(func() { s.fire(ctx, due.job, due.at) })
		}
		reload, alive := s.wait(ctx, now)
		if !alive {
			return nil
		}
		if now = s.cfg.Clock.Now(); reload || !now.Before(loaded.Add(s.cfg.Poll)) {
			s.load(false)
			loaded = now
		}
	}
}

// wait sleeps until the next run, a reload (true) or the end (alive
// false).
func (s *Scheduler) wait(ctx context.Context, now time.Time) (reload, alive bool) {
	select {
	case <-ctx.Done():
		return false, false
	case <-s.cfg.Clock.After(s.untilNext(now)):
		return false, true
	case <-s.reload:
		return true, true
	}
}

// load reads the jobs and plans each one's next run. On the first load a
// job that asks for it catches up a run missed while the scheduler was
// down; a job whose timetable changed is planned afresh.
func (s *Scheduler) load(first bool) {
	jobs, err := s.cfg.Store.Jobs()
	if err != nil {
		s.cfg.OnEvent(Event{Kind: JobInvalid, Err: err})
		return
	}
	now := s.cfg.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]planned{}
	for _, j := range jobs {
		when, err := j.Check()
		if err != nil {
			s.cfg.OnEvent(Event{Kind: JobInvalid, Job: j.Name, Err: err})
			continue
		}
		p, known := s.jobs[j.Name]
		if !known || p.job.When != j.When || p.job.TimeZone != j.TimeZone {
			p.next = s.firstRun(j, when, now, first)
		}
		p.job, p.when = j, when
		seen[j.Name] = p
	}
	s.jobs = seen
}

// firstRun is a job's first planned run: the next on its timetable, or
// now when it catches up a run missed since its last.
func (s *Scheduler) firstRun(j Job, when When, now time.Time, first bool) time.Time {
	if last := s.status[j.Name].LastFire; first && j.CatchUp && !last.IsZero() {
		if missed := when.Next(last); !missed.IsZero() && missed.Before(now) {
			return now
		}
	}
	return when.Next(now)
}

type dueRun struct {
	job Job
	at  time.Time
}

// due takes the jobs whose time has come and plans their next run from
// now: a scheduler that slept through several runs makes one, not all.
func (s *Scheduler) due(now time.Time) []dueRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []dueRun
	for name, p := range s.jobs {
		if p.next.IsZero() || p.next.After(now) {
			continue
		}
		at := p.next
		p.next = p.when.Next(now)
		s.jobs[name] = p
		if !p.job.Paused {
			out = append(out, dueRun{job: p.job, at: at})
		}
	}
	return out
}

// untilNext is the wait until the earliest planned run.
func (s *Scheduler) untilNext(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	wait := s.cfg.Poll
	for _, p := range s.jobs {
		if !p.next.IsZero() && !p.job.Paused {
			wait = min(wait, max(p.next.Sub(now), 0))
		}
	}
	return wait
}

// fire runs one job, unless its last run is still going.
func (s *Scheduler) fire(ctx context.Context, job Job, at time.Time) {
	if !s.begin(job.Name, at) {
		s.cfg.OnEvent(Event{Kind: JobSkipped, Job: job.Name, At: at})
		return
	}
	s.cfg.OnEvent(Event{Kind: JobStarted, Job: job.Name, At: at})
	out, err := s.cfg.Run(ctx, job)
	s.end(job.Name, out, err)
	kind := JobFinished
	if err != nil {
		kind = JobFailed
	}
	s.cfg.OnEvent(Event{Kind: kind, Job: job.Name, At: at, Run: out.Run, Err: err})
}

func (s *Scheduler) begin(name string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status[name]
	if s.running[name] {
		st.Skipped++
		s.status[name] = st
		s.save()
		return false
	}
	s.running[name] = true
	st.LastFire, st.LastRun, st.LastError, st.LastText = at, "", "", ""
	st.Runs++
	s.status[name] = st
	s.save()
	return true
}

func (s *Scheduler) end(name string, out Outcome, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, name)
	st := s.status[name]
	st.LastRun, st.LastEnd, st.LastText = out.Run, s.cfg.Clock.Now(), clip(out.Text)
	if err != nil {
		st.LastError = err.Error()
	}
	s.status[name] = st
	s.save()
}

// save writes the status; a failure is reported, the jobs keep running.
func (s *Scheduler) save() {
	if err := s.cfg.Store.saveStatus(s.status); err != nil {
		s.cfg.OnEvent(Event{Kind: JobInvalid, Err: err})
	}
}

// Status returns every job's status as the scheduler holds it.
func (s *Scheduler) Status() map[string]Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Status, len(s.status))
	for k, v := range s.status {
		out[k] = v
	}
	return out
}

// NextRun is the job's next planned run, zero when it has none.
func (s *Scheduler) NextRun(name string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[name].next
}

// clip keeps the first MaxText bytes of text, cut on a character.
func clip(text string) string {
	if len(text) <= MaxText {
		return text
	}
	cut := MaxText
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + " [...]"
}
