// hooks_test.go: before and after each automation's run
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"context"
	stderrors "errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agilira/go-errors"
)

// launchHooked runs a runner with hooks over store until the test ends.
func launchHooked(t *testing.T, store *Store, clock *fakeClock, rec *recorder, cfg Config) *Runner {
	t.Helper()
	cfg.Store, cfg.Run, cfg.OnEvent, cfg.Clock, cfg.Poll = store, rec.run, rec.onEvent, clock, time.Hour
	s, err := New(cfg)
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

func heard(name string) Trigger {
	return Trigger{Name: name, Kind: Message, Chat: "telegram:1", Prompt: "x"}
}

// BeforeRun changes what the run is told set it off; AfterRun hears the
// outcome before the status is written.
func TestHooksAroundARun(t *testing.T) {
	store, clock, rec := harness(t, heard("mail"))
	var mu sync.Mutex
	var after []Outcome
	var statusAtAfter Status
	var s *Runner
	s = launchHooked(t, store, clock, rec, Config{
		BeforeRun: func(_ context.Context, tr Trigger, f Fired) (Fired, error) {
			f.Detail = strings.ReplaceAll(f.Detail, "IBAN IT60X", "[iban]")
			f.Kind, f.At = Time, time.Time{} // not the hook's to change
			return f, nil
		},
		AfterRun: func(_ context.Context, tr Trigger, _ Fired, out Outcome, err error) {
			mu.Lock()
			defer mu.Unlock()
			after = append(after, out)
			statusAtAfter = s.Status()[tr.Name]
		},
	})
	if err := s.Fire("mail", "pay to IBAN IT60X now"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the run", func() bool { return rec.kinds("mail", Finished) == 1 })
	rec.mu.Lock()
	f := rec.fired[0]
	rec.mu.Unlock()
	if f.Detail != "pay to [iban] now" || f.Kind != Message || f.At.IsZero() {
		t.Fatalf("fired %+v", f)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(after) != 1 || after[0].Run != "r-mail" || statusAtAfter.LastRun != "" {
		t.Fatalf("after %+v, status then %+v", after, statusAtAfter)
	}
}

// BeforeRun refusing, or panicking, keeps the run from starting: the
// status says why, AfterRun is not told.
func TestBeforeRunRefuses(t *testing.T) {
	for name, hook := range map[string]func(context.Context, Trigger, Fired) (Fired, error){
		"error": func(context.Context, Trigger, Fired) (Fired, error) {
			return Fired{}, stderrors.New("outside office hours")
		},
		"panic": func(context.Context, Trigger, Fired) (Fired, error) { panic("host bug") },
	} {
		t.Run(name, func(t *testing.T) { refusedBy(t, hook) })
	}
}

// refusedBy fires a trigger whose BeforeRun is hook, and checks it did
// not run.
func refusedBy(t *testing.T, hook func(context.Context, Trigger, Fired) (Fired, error)) {
	store, clock, rec := harness(t, heard("mail"))
	told := false
	s := launchHooked(t, store, clock, rec, Config{BeforeRun: hook, AfterRun: func(context.Context, Trigger, Fired, Outcome, error) { told = true }})
	if err := s.Fire("mail", "x"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the refusal", func() bool { return rec.kinds("mail", Refused) == 1 })
	if st := s.Status()["mail"]; st.Refused != 1 || st.Runs != 0 || !strings.Contains(st.LastRefusal, "refused") {
		t.Fatalf("status %+v", st)
	}
	if rec.count() != 0 || told || s.busy("mail") {
		t.Fatalf("runs %d, told %v, busy %v", rec.count(), told, s.busy("mail"))
	}
	refusalsCoded(t, rec)
}

// refusalsCoded: every Refused event carries ErrRefused.
func refusalsCoded(t *testing.T, rec *recorder) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.events {
		if e.Kind == Refused && !errors.HasCode(e.Err, ErrRefused) {
			t.Fatalf("event %+v", e)
		}
	}
}

// RunNow runs a trigger of any kind, paused too, by hand; never twice at
// once.
func TestRunNow(t *testing.T) {
	tick := trigger("tick", "@daily")
	tick.Paused = true
	store, clock, rec := harness(t, tick)
	rec.block = make(chan struct{})
	s := launch(t, store, clock, rec)
	if err := s.RunNow("tick"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the run", func() bool { return rec.count() == 1 })
	if err := s.RunNow("tick"); !errors.HasCode(err, ErrRunning) {
		t.Fatalf("ran twice at once: %v", err)
	}
	close(rec.block)
	eventually(t, "the end", func() bool { return rec.kinds("tick", Finished) == 1 })
	rec.mu.Lock()
	f := rec.fired[0]
	rec.mu.Unlock()
	if !f.ByHand || f.Kind != Time || f.Detail != "" {
		t.Fatalf("fired %+v", f)
	}
	if err := s.RunNow("none"); !errors.HasCode(err, ErrNotFound) {
		t.Fatalf("ran a trigger that is not there: %v", err)
	}
	if err := s.Fire("tick", "x"); !errors.HasCode(err, ErrConfig) {
		t.Fatalf("Fire set off a time trigger: %v", err)
	}
}
