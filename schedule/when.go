// when.go: when a job runs — cron expressions and intervals
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package schedule

import (
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"
)

// When is a parsed timetable.
type When interface {
	// Next is the first time strictly after t when the job runs.
	Next(t time.Time) time.Time
	// String is the timetable as written.
	String() string
}

// MinInterval is the shortest @every: a model run more often than once a
// minute is a loop, not a schedule.
const MinInterval = time.Minute

// Parse reads a timetable: a cron expression of five fields (minute hour
// day-of-month month day-of-week, with *, lists, ranges, steps and names
// such as mon or jan), one of @hourly @daily @midnight @weekly @monthly
// @yearly @annually, or @every DURATION ("@every 10m", at least a minute).
// Times are in loc (time.Local when nil). When both day fields are
// restricted, either matching is enough, as in cron.
func Parse(spec string, loc *time.Location) (When, error) {
	if loc == nil {
		loc = time.Local
	}
	spec = strings.TrimSpace(spec)
	if d, ok := strings.CutPrefix(spec, "@every "); ok {
		return parseEvery(spec, strings.TrimSpace(d))
	}
	if expr, ok := descriptors[strings.ToLower(spec)]; ok {
		c, err := parseCron(expr, loc)
		if err != nil {
			return nil, err
		}
		c.text = spec
		return c, nil
	}
	c, err := parseCron(spec, loc)
	if err != nil {
		return nil, err
	}
	return c, nil
}

var descriptors = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

func badWhen(spec, why string) error {
	return errors.New(ErrConfig, "the timetable "+strconv.Quote(spec)+" is not valid: "+why)
}

// every runs at fixed intervals aligned to UTC, so that its times do not
// drift with restarts: @every 10m runs at :00, :10, :20...
type every struct {
	d    time.Duration
	text string
}

func parseEvery(spec, d string) (When, error) {
	dur, err := time.ParseDuration(d)
	if err != nil {
		return nil, badWhen(spec, "@every takes a duration such as 10m or 2h")
	}
	if dur < MinInterval || dur%time.Minute != 0 {
		return nil, badWhen(spec, "@every is whole minutes, at least one")
	}
	return every{d: dur, text: spec}, nil
}

func (e every) Next(t time.Time) time.Time {
	return t.Truncate(e.d).Add(e.d)
}

func (e every) String() string { return e.text }

// cron is a five-field expression: one bit per allowed value.
type cron struct {
	minute, hour, dom, month, dow uint64
	domAny, dowAny                bool
	loc                           *time.Location
	text                          string
}

type field struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
	fields   = []field{
		{name: "minute", min: 0, max: 59},
		{name: "hour", min: 0, max: 23},
		{name: "day of month", min: 1, max: 31},
		{name: "month", min: 1, max: 12, names: monthNames},
		{name: "day of week", min: 0, max: 7, names: dayNames}, // 7 is Sunday too
	}
)

func parseCron(spec string, loc *time.Location) (*cron, error) {
	parts := strings.Fields(spec)
	if len(parts) != 5 {
		return nil, badWhen(spec, "a cron expression has five fields: minute hour day-of-month month day-of-week")
	}
	var bits [5]uint64
	for i, p := range parts {
		b, err := parseField(p, fields[i])
		if err != nil {
			return nil, badWhen(spec, err.Error())
		}
		bits[i] = b
	}
	if bits[4]&(1<<7) != 0 {
		bits[4] |= 1 // 7 is Sunday
	}
	return &cron{minute: bits[0], hour: bits[1], dom: bits[2], month: bits[3], dow: bits[4],
		domAny: parts[2] == "*", dowAny: parts[4] == "*", loc: loc, text: spec}, nil
}

// parseField reads a comma list of *, N, A-B, each with an optional /STEP.
func parseField(text string, f field) (uint64, error) {
	var bits uint64
	for item := range strings.SplitSeq(text, ",") {
		b, err := parseItem(item, f)
		if err != nil {
			return 0, err
		}
		bits |= b
	}
	return bits, nil
}

func parseItem(item string, f field) (uint64, error) {
	rng, stepText, hasStep := strings.Cut(item, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepText)
		if err != nil || n < 1 || n > f.max {
			return 0, errors.New(ErrConfig, "bad step in the "+f.name+" field: "+item)
		}
		step = n
	}
	lo, hi, err := parseRange(rng, f, hasStep)
	if err != nil {
		return 0, err
	}
	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v) // #nosec G115 -- v is within 0..59
	}
	return bits, nil
}

// parseRange reads *, N or A-B; N/STEP means N to the field's end.
func parseRange(rng string, f field, stepped bool) (int, int, error) {
	if rng == "*" {
		return f.min, f.max, nil
	}
	a, b, isRange := strings.Cut(rng, "-")
	lo, err := value(a, f)
	if err != nil {
		return 0, 0, err
	}
	hi := lo
	switch {
	case isRange:
		if hi, err = value(b, f); err != nil {
			return 0, 0, err
		}
	case stepped:
		hi = f.max
	}
	if hi < lo {
		return 0, 0, errors.New(ErrConfig, "a range in the "+f.name+" field runs backwards: "+rng)
	}
	return lo, hi, nil
}

func value(text string, f field) (int, error) {
	if n, ok := f.names[strings.ToLower(text)]; ok {
		return n, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < f.min || n > f.max {
		return 0, errors.New(ErrConfig, "the "+f.name+" field takes "+strconv.Itoa(f.min)+"-"+strconv.Itoa(f.max)+": "+text)
	}
	return n, nil
}

func has(bits uint64, v int) bool { return bits&(1<<uint(v)) != 0 } // #nosec G115 -- v is a clock field

func (c *cron) String() string { return c.text }

// dayMatches applies cron's rule: with both day fields restricted, either
// one matching is enough.
func (c *cron) dayMatches(t time.Time) bool {
	dom, dow := has(c.dom, t.Day()), has(c.dow, int(t.Weekday()))
	if c.domAny || c.dowAny {
		return dom && dow
	}
	return dom || dow
}

// searchYears bounds the search: an expression such as "0 0 30 2 *"
// never matches, and Next then returns the zero time.
const searchYears = 5

// Next walks forward a field at a time, in the job's time zone: a local
// time skipped by a clock change is skipped, and a repeated one runs once
// (Next is always later than t).
func (c *cron) Next(t time.Time) time.Time {
	t = t.In(c.loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(searchYears, 0, 0)
	for t.Before(limit) {
		switch {
		case !has(c.month, int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, c.loc)
		case !c.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, c.loc)
		case !has(c.hour, t.Hour()):
			// by the clock, not by Truncate: some zones are half an hour off UTC
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, c.loc)
		case !has(c.minute, t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}
