// trigger.go: running agents when something happens
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package trigger runs agents when something happens: a time ("every
// weekday at 8"), a tool's answer that changes, files created, changed or
// removed, a message in a chat, a request to a webhook. Triggers live in a
// Store (a private directory: triggers.json people edit, status.json the
// runner writes); a Runner reads them, starts each run through Config.Run
// when it is set off, and records what became of it.
//
// The rules a runner keeps:
//   - one run of a trigger at a time: a time or a host's fire while the
//     last is still going is skipped and counted; a Tool or File trigger
//     does not look while its run goes, and takes note of what its path
//     holds after a File run (what the run wrote does not set it off);
//   - no burst after downtime: times missed while the runner was down, or
//     the machine asleep, are skipped, except one catch-up run for the
//     triggers that ask for it;
//   - a change is a change from what was last seen: the first look only
//     takes note, and what was seen is kept across restarts;
//   - one runner per store: a second one refuses to start;
//   - edits to the triggers are picked up while running (Reload, or
//     within Config.Poll).
//
// A trigger's run has no one in front of it: the host allows the tools
// the trigger was allowed when it was made (Trigger.Allow) and refuses the
// others. What set it off reaches the run as data from outside, never as
// instructions. The CLI runs triggers through `ringil serve`.
package trigger

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

// Run starts a trigger's run and waits for its end. It returns what the run
// produced and its error, nil for success. When ctx ends (the trigger's
// Timeout, or the runner stopping), the run must stop too.
type Run func(ctx context.Context, t Trigger, f Fired) (Outcome, error)

// Fired is what set a trigger off, for its run.
type Fired struct {
	Kind Kind
	// At is when: the time on the timetable, or when the change was seen,
	// the message heard, the request taken.
	At time.Time
	// Detail is what happened, as data from outside: the tool's new
	// answer, the files created, changed or removed, the message, the
	// request's body. Empty for a time. At most MaxDetail bytes.
	Detail string
	// ByHand: the user ran it now (RunNow), not what sets it off.
	ByHand bool
}

// MaxDetail bounds what a run is told of what set it off.
const MaxDetail = 16 << 10

// Call asks a tool for a Tool trigger: its answer as text.
type Call func(ctx context.Context, tool string, args json.RawMessage) (string, error)

// Outcome is what a run produced.
type Outcome struct {
	// Run is the run's ID with the host (serve keeps runs for a while).
	Run string
	// Text is the agent's answer; the status keeps its first MaxText bytes.
	Text string
}

// MaxText bounds the answer a trigger's status keeps.
const MaxText = 4 << 10

// EventKind is what happened to a trigger.
type EventKind string

const (
	Started  EventKind = "started"
	Finished EventKind = "finished"
	Failed   EventKind = "failed"
	Skipped  EventKind = "skipped"
	// Refused: Config.BeforeRun refused the run; it did not start.
	Refused EventKind = "refused"
	// LookFailed: a Tool or File trigger could not look; it looks again
	// at its next time.
	LookFailed EventKind = "look_failed"
	// Ended: the trigger's period is over; the runner removed it.
	Ended EventKind = "ended"
	// Invalid: the trigger cannot be read (its timetable, its zone, its
	// fields); it does not run until fixed.
	Invalid EventKind = "invalid"
)

// Event reports a trigger's run to Config.OnEvent.
type Event struct {
	Kind    EventKind
	Trigger string
	// At is when it was set off.
	At  time.Time
	Run string
	Err error
}

// Clock is time for the runner; tests replace it.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Config configures a Runner.
type Config struct {
	Store *Store
	Run   Run
	// Call asks the tools of Tool triggers; without it they cannot look.
	Call Call
	// OnEvent hears of every start, end and skip; it must not block.
	OnEvent func(Event)
	// BeforeRun, when set, is asked before each run: it returns what the
	// run is told set it off (as is, or changed: Detail redacted), or an
	// error that refuses the run (Refused, the error in the status). A
	// panic refuses it too.
	BeforeRun func(ctx context.Context, t Trigger, f Fired) (Fired, error)
	// AfterRun, when set, is told how each run that started ended: its
	// outcome, or its error (a timeout is ErrTimeout). It is called before
	// the status is written and the Finished or Failed event goes out; a
	// panic in it is ignored.
	AfterRun func(ctx context.Context, t Trigger, f Fired, out Outcome, err error)
	// Poll is how often the triggers are re-read; default 30s.
	Poll  time.Duration
	Clock Clock
}

// ErrTimeout: a run went past its trigger's Timeout and was stopped.
const ErrTimeout errors.ErrorCode = "TRIGGER_TIMEOUT"

// ErrStopped: the runner is not running (Fire, Heard).
const ErrStopped errors.ErrorCode = "TRIGGER_STOPPED"

// Runner runs a store's triggers.
type Runner struct {
	cfg    Config
	reload chan struct{}

	mu       sync.Mutex
	ctx      context.Context // Run's, while it runs; nil otherwise
	wg       sync.WaitGroup
	status   map[string]Status
	running  map[string]bool // looking or running
	triggers map[string]planned
	seen     map[string]snapshot // what File triggers last saw
	// shortLimit, in tests, replaces every run's Timeout.
	shortLimit time.Duration
}

// planned is a trigger as the runner holds it: next is zero for those
// the host sets off.
type planned struct {
	trigger Trigger
	when    When
	next    time.Time
}

// New checks the configuration.
func New(cfg Config) (*Runner, error) {
	if cfg.Store == nil || cfg.Run == nil {
		return nil, errors.New(ErrConfig, "a runner needs a store and a way to run triggers")
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
	return &Runner{cfg: cfg, reload: make(chan struct{}, 1), running: map[string]bool{},
		triggers: map[string]planned{}, seen: map[string]snapshot{}}, nil
}

// Reload re-reads the triggers now instead of at the next poll.
func (s *Runner) Reload() {
	select {
	case s.reload <- struct{}{}:
	default:
	}
}

// Run runs the triggers until ctx ends, then waits for the runs in
// progress (which end with ctx). It returns ErrBusy when another runner
// holds the store.
func (s *Runner) Run(ctx context.Context) error {
	release, err := s.cfg.Store.claim()
	if err != nil {
		return err
	}
	defer release()
	s.loadStatus()
	s.load(true)
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.ctx = nil
		s.mu.Unlock()
		s.wg.Wait()
	}()
	loaded := s.cfg.Clock.Now()
	for {
		now := s.cfg.Clock.Now()
		if s.anyDue(now) {
			// Edits from another process (a pause) cannot call Reload:
			// read them before anything starts.
			s.load(false)
			loaded = now
		}
		for _, due := range s.due(now) {
			s.wg.Go(func() { s.fire(ctx, due.trigger, due.at) })
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

// loadStatus reads what became of the triggers. A status that cannot be
// read is reported and started afresh: it is the runner's own record, and
// the triggers must not stop over it.
func (s *Runner) loadStatus() {
	st, err := s.cfg.Store.Status()
	if err != nil {
		s.cfg.OnEvent(Event{Kind: Invalid, Err: err})
		st = map[string]Status{}
	}
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

// anyDue reports whether a trigger's time has come.
func (s *Runner) anyDue(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.triggers {
		if !p.next.IsZero() && !p.next.After(now) {
			return true
		}
	}
	return false
}

// wait sleeps until the next run, a reload (true) or the end (alive
// false).
func (s *Runner) wait(ctx context.Context, now time.Time) (reload, alive bool) {
	select {
	case <-ctx.Done():
		return false, false
	case <-s.cfg.Clock.After(s.untilNext(now)):
		return false, true
	case <-s.reload:
		return true, true
	}
}

// load reads the triggers and plans each one's next time. On the first
// load a trigger that asks for it catches up a run missed while the
// runner was down; a trigger whose kind or timetable changed is planned
// afresh, and what it saw is forgotten.
func (s *Runner) load(first bool) {
	triggers, err := s.cfg.Store.Triggers()
	if err != nil {
		s.cfg.OnEvent(Event{Kind: Invalid, Err: err})
		return
	}
	now := s.cfg.Clock.Now()
	triggers = s.endPeriods(triggers, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := map[string]planned{}
	for _, t := range triggers {
		when, err := t.Check()
		if err != nil {
			s.cfg.OnEvent(Event{Kind: Invalid, Trigger: t.Name, Err: err})
			continue
		}
		s.own(t)
		p, known := s.triggers[t.Name]
		if !known || replanned(p.trigger, t) {
			p.next = s.firstRun(t, when, now, first)
		}
		p.trigger, p.when = t, when
		kept[t.Name] = p
	}
	s.triggers = kept
	s.prune(kept)
}

// endPeriods removes the triggers whose period is over (not while a run
// of theirs goes) and returns the others.
func (s *Runner) endPeriods(triggers []Trigger, at time.Time) []Trigger {
	kept := triggers[:0:0]
	for _, t := range triggers {
		s.mu.Lock()
		busy := s.running[t.Name]
		s.mu.Unlock()
		if !t.Ended(at) || busy {
			kept = append(kept, t)
			continue
		}
		if err := s.cfg.Store.RemoveIf(t.Name, func(now Trigger) bool { return now.Ended(at) }); err != nil {
			s.cfg.OnEvent(Event{Kind: Invalid, Trigger: t.Name, Err: err})
			kept = append(kept, t)
			continue
		}
		s.cfg.OnEvent(Event{Kind: Ended, Trigger: t.Name, At: at})
	}
	return kept
}

// own starts a fresh status for a trigger whose Target is not its
// status's: a new trigger by an old name, or one set off by something
// else now (whose old Seen and LastFire mean nothing); s.mu held.
func (s *Runner) own(t Trigger) {
	target := t.target()
	st, ok := s.status[t.Name]
	switch {
	case ok && st.Target == target:
		return
	case ok && st.Target == "": // a status from before targets were kept: its own
		st.Target = target
	default:
		st = Status{Target: target}
		delete(s.seen, t.Name)
	}
	s.status[t.Name] = st
	s.save()
}

// prune forgets the status of triggers no longer there; s.mu held.
func (s *Runner) prune(kept map[string]planned) {
	changed := false
	for name := range s.status {
		if _, ok := kept[name]; !ok && !s.running[name] {
			delete(s.status, name)
			delete(s.seen, name)
			changed = true
		}
	}
	if changed {
		s.save()
	}
}

// replanned: b sets off differently from a (kind, timetable, target).
func replanned(a, b Trigger) bool {
	return a.kind() != b.kind() || a.When != b.When || a.TimeZone != b.TimeZone || !sameTarget(a, b)
}

// sameTarget: a and b look at the same thing (tool and arguments, path).
func sameTarget(a, b Trigger) bool {
	return a.target() == b.target()
}

// firstRun is a trigger's first planned time: the next on its timetable,
// or now when it catches up a run missed since its last; zero for the
// triggers the host sets off.
func (s *Runner) firstRun(t Trigger, when When, now time.Time, first bool) time.Time {
	if when == nil {
		return time.Time{}
	}
	if last := s.status[t.Name].LastFire; first && t.CatchUp && t.kind() == Time && !last.IsZero() {
		if missed := when.Next(last); !missed.IsZero() && missed.Before(now) {
			return now
		}
	}
	return when.Next(now)
}

type dueRun struct {
	trigger Trigger
	at      time.Time
}

// due takes the triggers whose time has come and plans their next from
// now: a runner that slept through several times makes one, not all.
func (s *Runner) due(now time.Time) []dueRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []dueRun
	for name, p := range s.triggers {
		if p.next.IsZero() || p.next.After(now) {
			continue
		}
		at := p.next
		p.next = p.when.Next(now)
		s.triggers[name] = p
		if !p.trigger.Paused && p.trigger.Working(at) {
			out = append(out, dueRun{trigger: p.trigger, at: at})
		}
	}
	return out
}

// untilNext is the wait until the earliest planned time.
func (s *Runner) untilNext(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	wait := s.cfg.Poll
	for _, p := range s.triggers {
		if !p.next.IsZero() && !p.trigger.Paused {
			wait = min(wait, max(p.next.Sub(now), 0))
		}
	}
	return wait
}

// fire handles a trigger whose time has come: a Time trigger runs, a
// Tool or File trigger looks and runs if what it sees changed.
func (s *Runner) fire(ctx context.Context, t Trigger, at time.Time) {
	if t.kind() == Time {
		s.start(ctx, t, Fired{Kind: Time, At: at})
		return
	}
	if !s.hold(t.Name) {
		return // still looking, or its run still going: the next look sees the change
	}
	lookCtx, cancel := context.WithTimeout(ctx, lookTimeout)
	detail, changed, err := s.look(lookCtx, t)
	cancel()
	if err != nil || !changed {
		s.release(t.Name)
		if err != nil {
			s.cfg.OnEvent(Event{Kind: LookFailed, Trigger: t.Name, At: at, Err: err})
		}
		return
	}
	if s.run(ctx, t, Fired{Kind: t.kind(), At: s.cfg.Clock.Now(), Detail: clipTo(detail, MaxDetail)}) && t.kind() == File {
		s.rebase(t) // what its own run wrote does not set it off again
	}
}

// lookTimeout bounds a look: a tool that hangs fails the look, which is
// tried again at the next time.
const lookTimeout = 2 * time.Minute

// rebase takes note of what a File trigger's path holds after its run:
// the run may have written there, and that must not set it off again
// (nor, then, what others wrote there while it ran).
func (s *Runner) rebase(t Trigger) {
	if !s.hold(t.Name) {
		return
	}
	defer s.release(t.Name)
	if _, _, err := s.look(context.Background(), t); err != nil {
		s.cfg.OnEvent(Event{Kind: LookFailed, Trigger: t.Name, Err: err})
	}
}

// start runs a trigger, unless its last run is still going.
func (s *Runner) start(ctx context.Context, t Trigger, f Fired) {
	if !s.hold(t.Name) {
		s.skipped(t.Name)
		s.cfg.OnEvent(Event{Kind: Skipped, Trigger: t.Name, At: f.At})
		return
	}
	s.run(ctx, t, f)
}

// ErrRefused: Config.BeforeRun refused a run.
const ErrRefused errors.ErrorCode = "TRIGGER_REFUSED"

// run runs a trigger held for it; false when BeforeRun refused it.
func (s *Runner) run(ctx context.Context, t Trigger, f Fired) bool {
	f, err := s.beforeRun(ctx, t, f)
	if err != nil {
		s.refused(t.Name, f.At, err)
		s.cfg.OnEvent(Event{Kind: Refused, Trigger: t.Name, At: f.At, Err: err})
		return false
	}
	s.begin(t.Name, f.At)
	s.cfg.OnEvent(Event{Kind: Started, Trigger: t.Name, At: f.At})
	limit, _ := t.limit() // checked when loaded
	s.mu.Lock()
	if s.shortLimit > 0 {
		limit = s.shortLimit
	}
	s.mu.Unlock()
	runCtx, cancel := context.WithTimeout(ctx, limit)
	out, err := s.cfg.Run(runCtx, t, f)
	if runCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		err = errors.New(ErrTimeout, "stopped after "+limit.String())
	}
	cancel()
	s.afterRun(ctx, t, f, out, err)
	s.end(t.Name, out, err)
	kind := Finished
	if err != nil {
		kind = Failed
	}
	s.cfg.OnEvent(Event{Kind: kind, Trigger: t.Name, At: f.At, Run: out.Run, Err: err})
	return true
}

// beforeRun asks Config.BeforeRun; its Fired keeps the trigger's kind and
// time, and a Detail clipped as Fire clips it.
func (s *Runner) beforeRun(ctx context.Context, t Trigger, f Fired) (out Fired, err error) {
	if s.cfg.BeforeRun == nil {
		return f, nil
	}
	defer func() {
		if recover() != nil {
			out, err = f, errors.New(ErrRefused, "refused: the BeforeRun hook failed")
		}
	}()
	got, err := s.cfg.BeforeRun(ctx, t, f)
	if err != nil {
		return f, errors.Wrap(err, ErrRefused, "refused: "+clipTo(err.Error(), 300))
	}
	f.Detail = clipTo(got.Detail, MaxDetail)
	return f, nil
}

// afterRun tells Config.AfterRun; a panic in it is ignored.
func (s *Runner) afterRun(ctx context.Context, t Trigger, f Fired, out Outcome, err error) {
	if s.cfg.AfterRun == nil {
		return
	}
	defer func() { _ = recover() }()
	s.cfg.AfterRun(context.WithoutCancel(ctx), t, f, out, err)
}

// hold marks a trigger busy; false when it already is.
func (s *Runner) hold(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[name] {
		return false
	}
	s.running[name] = true
	return true
}

func (s *Runner) release(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, name)
}

func (s *Runner) skipped(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status[name]
	st.Skipped++
	s.status[name] = st
	s.save()
}

// refused records a run BeforeRun refused: not a run, the last one's
// record stays.
func (s *Runner) refused(name string, at time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, name)
	st := s.status[name]
	st.Refused++
	st.LastRefused, st.LastRefusal = at, clipTo(err.Error(), 500)
	s.status[name] = st
	s.save()
}

func (s *Runner) begin(name string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status[name]
	st.LastFire, st.LastRun, st.LastError, st.LastText = at, "", "", ""
	st.Runs++
	s.status[name] = st
	s.save()
}

func (s *Runner) end(name string, out Outcome, err error) {
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

// save writes the status; a failure is reported, the triggers keep
// running.
func (s *Runner) save() {
	if err := s.cfg.Store.saveStatus(s.status); err != nil {
		s.cfg.OnEvent(Event{Kind: Invalid, Err: err})
	}
}

// ErrRunning: a trigger set off while its last run is still going; that
// one is skipped (Fire says so, so that a webhook's sender may send again).
const ErrRunning errors.ErrorCode = "TRIGGER_RUNNING"

// Fire sets off a Message or Webhook trigger the host heard of, with
// what happened (clipped to MaxDetail). Its run goes on after Fire
// returns; one set off while the last is still going is skipped, and
// Fire returns ErrRunning.
func (s *Runner) Fire(name, detail string) error { return s.fireNow(name, detail, false) }

// RunNow runs a trigger of any kind now, because the user asked (an
// app's "run now"): paused or outside its period too, never twice at
// once (ErrRunning). Its Fired is ByHand, with no Detail.
func (s *Runner) RunNow(name string) error { return s.fireNow(name, "", true) }

func (s *Runner) fireNow(name, detail string, byHand bool) (err error) {
	started := false
	s.mu.Lock()
	ctx := s.ctx
	p, ok := s.triggers[name]
	if ctx != nil {
		s.wg.Add(1) // under s.mu: Run stops taking runs (ctx nil) before it waits
	}
	s.mu.Unlock()
	if ctx != nil {
		defer func() {
			if !started {
				s.wg.Done()
			}
		}()
	}
	if ctx == nil {
		return errors.New(ErrStopped, "the triggers are not running")
	}
	if !ok {
		return errors.New(ErrNotFound, "no trigger "+name)
	}
	if !byHand {
		if err := s.fireable(p.trigger); err != nil {
			return err
		}
	}
	f := Fired{Kind: p.trigger.kind(), At: s.cfg.Clock.Now(), Detail: clipTo(detail, MaxDetail), ByHand: byHand}
	if !s.hold(name) { // here, not in the run: the caller hears of it
		s.skipped(name)
		s.cfg.OnEvent(Event{Kind: Skipped, Trigger: name, At: f.At})
		return errors.New(ErrRunning, "trigger "+name+" is still running its last run")
	}
	started = true
	go s.runHeld(ctx, p.trigger, f)
	return nil
}

// runHeld runs a trigger held for it by Fire or RunNow, counted in wg; a
// File trigger then takes note of what its run wrote, so that it does not
// set itself off again.
func (s *Runner) runHeld(ctx context.Context, t Trigger, f Fired) {
	defer s.wg.Done()
	if s.run(ctx, t, f) && t.kind() == File {
		s.rebase(t)
	}
}

// fireable: t is set off by the host, and may be now.
func (s *Runner) fireable(t Trigger) error {
	switch {
	case t.kind() != Message && t.kind() != Webhook:
		return errors.New(ErrConfig, "trigger "+t.Name+" is set off by its "+string(t.kind())+", not by the host")
	case t.Paused:
		return errors.New(ErrConfig, "trigger "+t.Name+" is paused")
	case !t.Working(s.cfg.Clock.Now()):
		return errors.New(ErrConfig, "trigger "+t.Name+" is outside its period")
	}
	return nil
}

// Listens reports whether a Message trigger listens to chat, so that a
// host passes on (Heard) only the messages of chats listened to.
func (s *Runner) Listens(chat string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.triggers {
		if p.trigger.kind() == Message && !p.trigger.Paused && p.trigger.Chat == chat {
			return true
		}
	}
	return false
}

// Heard sets off the Message triggers of chat whose words are in text
// (letter case aside), and returns their names.
func (s *Runner) Heard(chat, text string) []string {
	s.mu.Lock()
	var names []string
	for name, p := range s.triggers {
		t := p.trigger
		if t.kind() == Message && !t.Paused && t.Chat == chat && strings.Contains(strings.ToLower(text), strings.ToLower(t.Match)) {
			names = append(names, name)
		}
	}
	s.mu.Unlock()
	var fired []string
	for _, name := range names {
		if s.Fire(name, text) == nil {
			fired = append(fired, name)
		}
	}
	return fired
}

// Status returns every trigger's status as the runner holds it.
func (s *Runner) Status() map[string]Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Status, len(s.status))
	for k, v := range s.status {
		out[k] = v
	}
	return out
}

// NextRun is the trigger's next planned time, zero when it has none.
func (s *Runner) NextRun(name string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.triggers[name].next
}

// clip keeps the first MaxText bytes of text, cut on a character.
func clip(text string) string {
	return clipTo(text, MaxText)
}

// clipTo keeps the first max bytes of text, cut on a character.
func clipTo(text string, max int) string {
	if len(text) <= max {
		return text
	}
	const more = " [...]"
	cut := max - len(more)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + more
}
