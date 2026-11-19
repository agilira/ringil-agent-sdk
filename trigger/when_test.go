// when_test.go: timetables
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"testing"
	"time"

	"github.com/agilira/go-errors"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skip(err)
	}
	return loc
}

func TestCronNext(t *testing.T) {
	utc := time.UTC
	from := time.Date(2026, 11, 18, 14, 37, 20, 0, utc) // a Wednesday
	for spec, want := range map[string]string{
		"* * * * *":       "2026-11-18 14:38",
		"0 8 * * *":       "2026-11-19 08:00",
		"0 8 * * 1-5":     "2026-11-19 08:00",
		"0 8 * * mon":     "2026-11-23 08:00",
		"*/15 * * * *":    "2026-11-18 14:45",
		"5-10/2 * * * *":  "2026-11-18 15:05",
		"0 0 1 * *":       "2026-12-01 00:00",
		"0 0 1 jan *":     "2027-01-01 00:00",
		"0 12 * * 7":      "2026-11-22 12:00", // 7 is Sunday
		"0 12 13 * fri":   "2026-11-20 12:00", // either day field
		"0 9 29 2 *":      "2028-02-29 09:00",
		"@hourly":         "2026-11-18 15:00",
		"@daily":          "2026-11-19 00:00",
		"@weekly":         "2026-11-22 00:00",
		"30 14,16 * * *":  "2026-11-18 16:30",
		"37 14 18 11 wed": "2026-11-25 14:37", // strictly after; either day field: next Wednesday
	} {
		w, err := Parse(spec, utc)
		if err != nil {
			t.Errorf("%s: %v", spec, err)
			continue
		}
		if got := w.Next(from).Format("2006-01-02 15:04"); got != want {
			t.Errorf("%s: %s, want %s", spec, got, want)
		}
	}
}

func TestCronNeverMatches(t *testing.T) {
	w, err := Parse("0 0 30 2 *", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Next(time.Now()).IsZero() {
		t.Fatal("February 30th found")
	}
}

func TestCronAcrossClockChanges(t *testing.T) {
	rome := mustZone(t, "Europe/Rome")
	w, _ := Parse("30 2 * * *", rome)
	// 29 March 2026: 02:00 becomes 03:00, there is no 02:30.
	got := w.Next(time.Date(2026, 3, 28, 12, 0, 0, 0, rome))
	if got.Format("2006-01-02 15:04") != "2026-03-30 02:30" {
		t.Errorf("spring forward: %v", got)
	}
	// 25 October 2026: 02:30 happens twice; the trigger runs once that day.
	first := w.Next(time.Date(2026, 10, 25, 0, 0, 0, 0, rome))
	second := w.Next(first)
	if first.Day() != 25 || second.Day() != 26 {
		t.Errorf("fall back: %v then %v", first, second)
	}
	kolkata := mustZone(t, "Asia/Kolkata") // half an hour off UTC
	h, _ := Parse("0 * * * *", kolkata)
	if got := h.Next(time.Date(2026, 1, 1, 10, 10, 0, 0, kolkata)); got.Format("15:04") != "11:00" {
		t.Errorf("half-hour zone: %v", got)
	}
}

func TestEvery(t *testing.T) {
	w, err := Parse("@every 10m", nil)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 1, 1, 10, 3, 0, 0, time.UTC)
	if got := w.Next(from); !got.Equal(time.Date(2026, 1, 1, 10, 10, 0, 0, time.UTC)) {
		t.Errorf("%v", got)
	}
	if got := w.Next(time.Date(2026, 1, 1, 10, 10, 0, 0, time.UTC)); got.Minute() != 20 {
		t.Errorf("not strictly after: %v", got)
	}
}

func TestParseRefuses(t *testing.T) {
	for _, bad := range []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *",
		"* * * 13 *", "* * * * 8", "5-1 * * * *", "*/0 * * * *", "a * * * *", "@every 30s", "@every 90s",
		"@every soon", "@often", "1,,2 * * * *"} {
		if _, err := Parse(bad, time.UTC); !errors.HasCode(err, ErrConfig) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}
