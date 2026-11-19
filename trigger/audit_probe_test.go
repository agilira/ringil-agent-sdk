// audit_probe_test.go: Next against a minute-by-minute search (audit 18/11)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"testing"
	"time"
)

// oracle walks instants a minute at a time and takes the first whose wall
// reading matches and is that reading's first occurrence: a repeated
// local time runs once.
func oracle(c *cron, from time.Time) time.Time {
	u := from.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 400*24*60; i++ {
		l := u.In(c.loc)
		if has(c.month, int(l.Month())) && c.dayMatches(l) && has(c.hour, l.Hour()) && has(c.minute, l.Minute()) && firstReading(u, c.loc) {
			return u
		}
		u = u.Add(time.Minute)
	}
	return time.Time{}
}

// firstReading: no earlier instant within three hours reads the same.
func firstReading(u time.Time, loc *time.Location) bool {
	wall := civil(u.In(loc))
	for d := time.Minute; d <= 3*time.Hour; d += time.Minute {
		if civil(u.Add(-d).In(loc)).Equal(wall) {
			return false
		}
	}
	return true
}

func TestAuditNextMatchesBruteForce(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("a long search; run without -race and -short")
	}
	zones := []string{"America/Santiago", "America/Havana", "Asia/Beirut", "America/Asuncion", "Asia/Gaza", "Australia/Lord_Howe", "Europe/Rome", "America/New_York", "Asia/Kolkata", "Pacific/Chatham", "America/St_Johns", "Africa/Cairo", "America/Scoresbysund", "Atlantic/Azores", "Europe/London", "Australia/Sydney", "Antarctica/Troll"}
	specs := []string{"0 0 * * *", "30 0 * * *", "0 0 1 * *", "15 2 * * *", "30 2 * * *", "0 3 * * *", "45 1 * * *", "0 * * * *", "*/20 * * * *", "0 9 * * 1-5", "0 23 * * *"}
	for _, zn := range zones {
		loc, err := time.LoadLocation(zn)
		if err != nil {
			continue
		}
		for _, sp := range specs {
			probeYear(t, zn, sp, loc)
		}
	}
}

// probeYear follows a timetable through 2026, run by run.
func probeYear(t *testing.T, zn, sp string, loc *time.Location) {
	t.Helper()
	w, _ := Parse(sp, loc)
	c := w.(*cron)
	for cur := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); cur.Year() < 2027; {
		done := make(chan time.Time, 1)
		f := cur
		go func() { done <- w.Next(f) }()
		var n time.Time
		select {
		case n = <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("HANG %s %q from %v", zn, sp, f.In(loc))
			return
		}
		want := oracle(c, f)
		if !n.Equal(want) {
			t.Errorf("WRONG %s %q from %v: got %v want %v", zn, sp, f.In(loc), n.In(loc), want.In(loc))
			n = want
		}
		cur = n
	}
}
