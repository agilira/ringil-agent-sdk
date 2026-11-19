// audit_sched_test.go: regressions of the runner (audit 18/11)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"context"
	"testing"
	"time"
)

// `ringil schedule pause` edits triggers.json from another process, which
// cannot call Reload: a run due before the next poll still starts.
func TestAuditPausedJobRunsBeforeNextPoll(t *testing.T) {
	store, clock, rec := harness(t, trigger("tick", "@every 10m"))
	launch(t, store, clock, rec) // Poll is an hour in the harness; serve uses 30s
	if err := store.Pause("tick", true); err != nil {
		t.Fatal(err)
	}
	clock.advance(7 * time.Minute) // 10:10, due
	clock.settle(t)
	time.Sleep(50 * time.Millisecond)
	if n := rec.kinds("tick", Started); n != 0 {
		t.Fatalf("paused trigger started %d time(s)", n)
	}
}

// status.json is only the runner's record; a bad one (a crash of the
// disk, a hand edit, a field from a newer version) stops every trigger.
func TestAuditBadStatusStopsAllJobs(t *testing.T) {
	store, clock, rec := harness(t, trigger("tick", "@every 10m"))
	if err := store.write(statusFile, map[string]any{"tick": map[string]any{"runs": 1, "new_field": true}}); err != nil {
		t.Fatal(err)
	}
	s, _ := New(Config{Store: store, Run: rec.run, OnEvent: rec.onEvent, Clock: clock})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("the runner gave up: %v", err)
	case <-clock.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("neither ran nor failed")
	}
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

// Status may be called while Run starts: Run assigns s.status without
// the lock (run with -race).
func TestAuditStatusRacesRun(t *testing.T) {
	store, clock, rec := harness(t, trigger("tick", "@every 10m"))
	s, _ := New(Config{Store: store, Run: rec.run, Clock: clock})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	for range 1000 {
		_ = s.Status()
	}
	clock.settle(t)
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

// A trigger whose Next does not return (see TestAuditNextHangsOnMidnightGap)
// wedges load under s.mu: no other trigger runs and Status blocks.
func TestAuditOneBadZoneWedgesScheduler(t *testing.T) {
	store, _, rec := harness(t, trigger("other", "@every 10m"),
		Trigger{Name: "chile", When: "0 0 * * *", TimeZone: "America/Santiago", Prompt: "x"})
	clock := newClock(time.Date(2026, 9, 5, 16, 0, 0, 0, time.UTC))
	s, _ := New(Config{Store: store, Run: rec.run, Clock: clock})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	select {
	case <-clock.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("the runner never got past loading the triggers")
	}
}
