// schedule_test.go: the scheduler's rules, on a fake clock
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package schedule

import (
	"context"
	stderrors "errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

// fakeClock moves only when told; After channels fire as it passes them.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
	waiting chan struct{} // signalled each time the scheduler waits
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{now: t, waiting: make(chan struct{}, 100)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
	} else {
		c.waiters = append(c.waiters, waiter{at: c.now.Add(d), ch: ch})
	}
	c.waiting <- struct{}{}
	return ch
}

// advance moves the clock and fires what is due.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

// settle waits until the scheduler is waiting on the clock again.
func (c *fakeClock) settle(t *testing.T) {
	t.Helper()
	select {
	case <-c.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler did not come back to wait")
	}
}

type recorder struct {
	mu     sync.Mutex
	runs   []string
	block  chan struct{} // runs wait on it when set
	events []Event
}

func (r *recorder) run(ctx context.Context, j Job) (Outcome, error) {
	r.mu.Lock()
	r.runs = append(r.runs, j.Name)
	block := r.block
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	return Outcome{Run: "r-" + j.Name, Text: "done " + j.Name}, nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

func (r *recorder) onEvent(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) kinds(job string, kind EventKind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Job == job && e.Kind == kind {
			n++
		}
	}
	return n
}

var start = time.Date(2026, 11, 18, 10, 3, 0, 0, time.UTC)

// harness runs a scheduler over a fresh store until the test ends.
func harness(t *testing.T, jobs ...Job) (*Store, *fakeClock, *recorder) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, j := range jobs {
		if err := store.Put(j); err != nil {
			t.Fatal(err)
		}
	}
	return store, newClock(start), &recorder{}
}

func launch(t *testing.T, store *Store, clock *fakeClock, rec *recorder) *Scheduler {
	t.Helper()
	s, err := New(Config{Store: store, Run: rec.run, OnEvent: rec.onEvent, Clock: clock, Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	clock.settle(t)
	return s
}

// eventually waits for cond, which goroutines make true.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 500 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(what)
}

func job(name, when string) Job { return Job{Name: name, When: when, Input: "do it"} }

func TestRunsOnTimeAndOnlyOnce(t *testing.T) {
	store, clock, rec := harness(t, job("tick", "@every 10m"))
	s := launch(t, store, clock, rec)
	clock.advance(6 * time.Minute) // 10:09: not yet, nothing wakes
	time.Sleep(20 * time.Millisecond)
	if rec.count() != 0 {
		t.Fatal("ran early")
	}
	clock.advance(time.Minute) // 10:10
	clock.settle(t)
	eventually(t, "the run", func() bool { return rec.kinds("tick", JobFinished) == 1 })
	st := s.Status()["tick"]
	if st.Runs != 1 || st.LastRun != "r-tick" || st.LastText != "done tick" || !st.LastFire.Equal(start.Add(7*time.Minute)) {
		t.Fatalf("status %+v", st)
	}
	saved, _ := store.Status()
	if saved["tick"].LastRun != "r-tick" {
		t.Fatalf("status not saved: %+v", saved)
	}
}

func TestNoBurstAfterSleep(t *testing.T) {
	store, clock, rec := harness(t, job("tick", "@every 10m"))
	launch(t, store, clock, rec)
	clock.advance(3 * time.Hour) // the machine slept through 18 runs
	clock.settle(t)
	eventually(t, "one run", func() bool { return rec.kinds("tick", JobFinished) == 1 })
	time.Sleep(50 * time.Millisecond)
	if rec.count() != 1 {
		t.Fatalf("%d runs after a long sleep", rec.count())
	}
}

func TestOverlappingRunIsSkipped(t *testing.T) {
	store, clock, rec := harness(t, job("slow", "@every 10m"))
	rec.block = make(chan struct{})
	s := launch(t, store, clock, rec)
	clock.advance(7 * time.Minute)
	clock.settle(t)
	eventually(t, "first run", func() bool { return rec.count() == 1 })
	clock.advance(10 * time.Minute) // due again, still running
	clock.settle(t)
	eventually(t, "the skip", func() bool { return rec.kinds("slow", JobSkipped) == 1 })
	close(rec.block)
	eventually(t, "the end", func() bool { return rec.kinds("slow", JobFinished) == 1 })
	if st := s.Status()["slow"]; st.Skipped != 1 || st.Runs != 1 {
		t.Fatalf("status %+v", st)
	}
}

func TestCatchUpOnlyWhenAsked(t *testing.T) {
	store, clock, rec := harness(t, Job{Name: "eager", When: "@hourly", Input: "x", CatchUp: true}, job("lazy", "@hourly"))
	// Both last ran two hours before the scheduler started.
	if err := store.saveStatus(map[string]Status{"eager": {LastFire: start.Add(-2 * time.Hour)}, "lazy": {LastFire: start.Add(-2 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	launch(t, store, clock, rec)
	eventually(t, "the catch-up", func() bool { return rec.kinds("eager", JobFinished) == 1 })
	time.Sleep(50 * time.Millisecond)
	if rec.kinds("lazy", JobStarted) != 0 {
		t.Fatal("a job without catch-up ran a missed run")
	}
}

func TestPausedReloadAndInvalid(t *testing.T) {
	store, clock, rec := harness(t, Job{Name: "off", When: "@every 10m", Input: "x", Paused: true})
	s := launch(t, store, clock, rec)
	if err := store.Put(job("new", "@every 10m")); err != nil {
		t.Fatal(err)
	}
	s.Reload()
	clock.settle(t)
	clock.advance(7 * time.Minute)
	clock.settle(t)
	eventually(t, "the new job", func() bool { return rec.kinds("new", JobFinished) == 1 })
	if rec.kinds("off", JobStarted) != 0 {
		t.Fatal("a paused job ran")
	}
	if !s.NextRun("new").Equal(start.Add(17 * time.Minute)) {
		t.Fatalf("next %v", s.NextRun("new"))
	}
}

func TestOneSchedulerPerStore(t *testing.T) {
	store, clock, rec := harness(t)
	launch(t, store, clock, rec)
	other, err := OpenStore(store.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	s2, _ := New(Config{Store: other, Run: rec.run, Clock: clock})
	if err := s2.Run(context.Background()); !errors.HasCode(err, ErrBusy) {
		t.Fatalf("second scheduler: %v", err)
	}
}

func TestStoreEdits(t *testing.T) {
	store, _, _ := harness(t, job("a", "@daily"))
	for _, bad := range []Job{
		{Name: "bad name", When: "@daily", Input: "x"},
		{Name: "b", When: "@daily"},
		{Name: "b", When: "nope", Input: "x"},
		{Name: "b", When: "@daily", Input: "x", TimeZone: "Mars/Olympus"},
		{Name: "b", When: "@daily", Input: "x", Conversation: "../x"},
	} {
		if err := store.Put(bad); !errors.HasCode(err, ErrConfig) {
			t.Errorf("%+v accepted: %v", bad, err)
		}
	}
	if err := store.Pause("a", true); err != nil {
		t.Fatal(err)
	}
	jobs, _ := store.Jobs()
	if len(jobs) != 1 || !jobs[0].Paused {
		t.Fatalf("%+v", jobs)
	}
	if err := store.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("a"); !errors.HasCode(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	var coded *errors.Error
	if err := store.Pause("zz", true); !stderrors.As(err, &coded) {
		t.Fatalf("%v", err)
	}
}

func TestClipKeepsCharacters(t *testing.T) {
	long := strings.Repeat("è", MaxText)
	got := clip(long)
	if !utf8.ValidString(got) || len(got) > MaxText+6 {
		t.Fatalf("%d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}
