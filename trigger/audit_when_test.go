// audit_when_test.go: regressions of timetables (audit 18/11)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"testing"
	"time"
)

func nextWithin(t *testing.T, w When, from time.Time) time.Time {
	t.Helper()
	done := make(chan time.Time, 1)
	go func() { done <- w.Next(from) }()
	select {
	case n := <-done:
		return n
	case <-time.After(2 * time.Second):
		t.Fatalf("Next(%v) of %q does not return", from, w)
		return time.Time{}
	}
}

// Chile skips local midnight when DST starts (2026-09-06 00:00 -> 01:00):
// time.Date(day+1, 0:00) normalizes back to 23:00 of the day before, and
// Next loops for ever without reaching its limit.
func TestAuditNextHangsOnMidnightGap(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skip(err)
	}
	w, err := Parse("0 0 * * *", loc)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 5, 12, 0, 0, 0, loc)
	n := nextWithin(t, w, from)
	if n.IsZero() || !n.After(from) {
		t.Fatalf("Next = %v", n)
	}
}

// The doc says a repeated local time runs once; "30 1 * * *" runs at
// 01:30 EDT and again at 01:30 EST on the fall-back night.
func TestAuditNextRunsTwiceOnFallBack(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	w, _ := Parse("30 1 * * *", loc)
	first := nextWithin(t, w, time.Date(2026, 11, 1, 0, 0, 0, 0, loc))
	second := nextWithin(t, w, first)
	if second.Sub(first) < 12*time.Hour {
		t.Fatalf("runs at %v and again at %v", first, second)
	}
}

// Feb 29 is up to 8 years away (2096 -> 2104); the 5-year search says never.
func TestAuditNextFeb29AcrossCentury(t *testing.T) {
	w, _ := Parse("0 0 29 2 *", time.UTC)
	n := nextWithin(t, w, time.Date(2097, 3, 1, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2104, 2, 29, 0, 0, 0, 0, time.UTC); !n.Equal(want) {
		t.Fatalf("Next = %v, want %v", n, want)
	}
}

// Every zone west of UTC with DST: time.Date(1 AM + 1h) on the
// spring-forward day lands on 1:00 EST again, and Next spins for ever.
func TestAuditNextHangsOnSpringForwardNewYork(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	w, _ := Parse("0 9 * * *", loc)
	from := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)
	if n := nextWithin(t, w, from); !n.Equal(time.Date(2026, 3, 8, 9, 0, 0, 0, loc)) {
		t.Fatalf("Next = %v", n)
	}
}

// Vixie cron treats a day field starting with * (e.g. */2) as unrestricted
// for the either-rule: "0 0 */2 * 1" is odd days that are Mondays there,
// here every odd day plus every Monday.
func TestAuditStarStepDayFieldIsEitherRule(t *testing.T) {
	w, _ := Parse("0 0 */2 * 1", time.UTC)
	// 2026-11-19 is a Thursday and an odd day.
	if n := w.Next(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)); n.Weekday() != time.Monday {
		t.Fatalf("Next = %v (%v)", n, n.Weekday())
	}
}
