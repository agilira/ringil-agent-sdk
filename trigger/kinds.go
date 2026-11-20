// kinds.go: what a trigger is, and what sets it off
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agilira/go-errors"
)

// Kind is what sets a trigger off.
type Kind string

const (
	// Time: its timetable (When), "every weekday at 8".
	Time Kind = "time"
	// Tool: a tool's answer, asked with the same arguments every When
	// (default every 5 minutes), changed since it was last asked.
	Tool Kind = "tool"
	// File: a file, or the files of a directory, created, changed or
	// removed since they were last looked at, every When (default every
	// minute).
	File Kind = "file"
	// Message: a message in a chat (the host calls Runner.Fire).
	Message Kind = "message"
	// Webhook: a request to the host's endpoint (the host checks the
	// Secret and calls Runner.Fire).
	Webhook Kind = "webhook"
)

// Trigger is a run of an agent set off by something: a time, a change, a
// message, a request. The struct is the Go API; create_trigger makes
// the kinds the agent may (CreateTool).
type Trigger struct {
	// Name identifies the trigger: 1-48 letters, digits, - and _.
	Name string `json:"name"`
	// Kind is what sets it off; empty is Time.
	Kind Kind `json:"kind,omitempty"`
	// When is the timetable (see Parse) of a Time trigger, and how often a
	// Tool or File trigger looks ("@every 5m").
	When string `json:"when,omitempty"`
	// TimeZone is an IANA zone ("Europe/Rome") for When; empty is local.
	TimeZone string `json:"time_zone,omitempty"`
	// CatchUp runs a Time trigger once when the runner starts if a run was
	// missed while it was down; off, missed runs are skipped.
	CatchUp bool `json:"catch_up,omitempty"`
	// Tool and Args: the tool a Tool trigger asks, and with what.
	Tool string          `json:"tool,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
	// Path is the file or directory (its own files, not deeper) a File
	// trigger looks at: absolute.
	Path string `json:"path,omitempty"`
	// Chat is the chat a Message trigger listens to (the host's own
	// name for it), and Match the words a message must have to set it
	// off (any message when empty; letter case does not count).
	Chat  string `json:"chat,omitempty"`
	Match string `json:"match,omitempty"`
	// Secret is what a Webhook's requests are signed with (the host
	// checks it): at least 32 characters.
	Secret string `json:"secret,omitempty"`
	// Skill is the skill the run follows, and Prompt what else it is
	// told; at least one. What set the trigger off comes after them, as
	// data.
	Skill  string `json:"skill,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// Profile is the role that runs it; empty is "default".
	Profile string `json:"profile,omitempty"`
	// Continue carries on one conversation across runs (the agent
	// remembers what it did last time); off, each run starts afresh.
	Continue bool `json:"continue,omitempty"`
	// Allow are the tools its runs may use without asking, allowed when
	// it was made: the host refuses any other call that would ask.
	Allow []string `json:"allow,omitempty"`
	// Deliver is where the host sends what a run answers (the host's
	// name for a chat); empty keeps it in the status.
	Deliver string `json:"deliver,omitempty"`
	// From and Until bound the period it works in: "2026-12-01", or
	// "2026-12-01 09:00", in TimeZone. Before From it does not set off;
	// after Until it has ended, and the runner removes it. A date alone
	// as Until counts its whole day. Empty: from now, for ever.
	From  string `json:"from,omitempty"`
	Until string `json:"until,omitempty"`
	// Timeout bounds a run ("30m"): past it, the run is stopped. Default
	// 10 minutes, at most 2 hours: a trigger's run has no one in front of
	// it to stop it.
	Timeout string `json:"timeout,omitempty"`
	// Paused triggers keep their place but do not run.
	Paused bool `json:"paused,omitempty"`
}

// Bounds of a trigger.
const (
	maxAllow    = 64
	minSecret   = 32
	maxShort    = 512 // Tool, Path, Chat, Match, Secret, Deliver
	defaultTool = "@every 5m"
	defaultFile = "@every 1m"
)

var (
	validName  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`)
	validSkill = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	validTool  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// target is a digest of what sets the trigger off.
func (t Trigger) target() string {
	var args bytes.Buffer
	if json.Compact(&args, t.Args) != nil {
		args.Write(t.Args)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{string(t.kind()), t.When, t.TimeZone, t.Tool, args.String(), t.Path, t.Chat, t.Match}, "\x00")))
	return hex.EncodeToString(sum[:12])
}

// kind is the trigger's kind, Time when empty.
func (t Trigger) kind() Kind {
	if t.Kind == "" {
		return Time
	}
	return t.Kind
}

// Check validates the trigger and returns when it looks or runs: its
// timetable for Time, Tool and File, nil for Message and Webhook (they
// are set off by the host).
func (t Trigger) Check() (When, error) {
	if !validName.MatchString(t.Name) {
		return nil, errors.New(ErrConfig, "trigger names are 1-48 letters, digits, - and _")
	}
	if err := t.checkRun(); err != nil {
		return nil, errors.Wrap(err, ErrConfig, "trigger "+t.Name+": "+err.Error())
	}
	if err := t.checkKind(); err != nil {
		return nil, errors.Wrap(err, ErrConfig, "trigger "+t.Name+": "+err.Error())
	}
	return t.when()
}

// checkRun checks what a run does.
func (t Trigger) checkRun() error {
	switch {
	case t.Skill == "" && strings.TrimSpace(t.Prompt) == "":
		return errors.New(ErrConfig, "a skill or a prompt is needed")
	case t.Skill != "" && !validSkill.MatchString(t.Skill):
		return errors.New(ErrConfig, "skill names are 1-64 lower-case letters, digits and -")
	case len(t.Prompt) > MaxInput:
		return errors.New(ErrConfig, "the prompt is larger than 64 KiB")
	case len(t.Deliver) > maxShort:
		return errors.New(ErrConfig, "the delivery is too long")
	}
	if _, err := t.limit(); err != nil {
		return err
	}
	if _, _, err := t.period(); err != nil {
		return err
	}
	return t.checkAllow()
}

func (t Trigger) checkAllow() error {
	if len(t.Allow) > maxAllow {
		return errors.New(ErrConfig, "at most 64 tools are allowed")
	}
	for _, name := range t.Allow {
		if !validTool.MatchString(name) {
			return errors.New(ErrConfig, "not a tool name: "+name)
		}
	}
	return nil
}

// period is when the trigger works: zero From is from always, zero Until
// for ever.
func (t Trigger) period() (from, until time.Time, err error) {
	loc, err := zone(t.TimeZone)
	if err != nil {
		return from, until, errors.New(ErrConfig, "unknown time zone "+t.TimeZone)
	}
	if from, _, err = moment(t.From, loc); err != nil {
		return from, until, err
	}
	var day bool
	if until, day, err = moment(t.Until, loc); err != nil {
		return from, until, err
	}
	if day {
		until = until.AddDate(0, 0, 1) // the whole day
	}
	if !from.IsZero() && !until.IsZero() && !until.After(from) {
		return from, until, errors.New(ErrConfig, "until comes before from")
	}
	return from, until, nil
}

// moment reads "2026-12-01", "2026-12-01 09:00" or RFC 3339 in loc; day
// says it was a date alone.
func moment(s string, loc *time.Location) (time.Time, bool, error) {
	if s == "" {
		return time.Time{}, false, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, true, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, false, nil
		}
	}
	return time.Time{}, false, errors.New(ErrConfig, "from and until are dates, 2026-12-01 or 2026-12-01 09:00: not "+s)
}

// Working says whether the trigger works at now: within its period.
func (t Trigger) Working(now time.Time) bool {
	from, until, err := t.period()
	return err == nil && (from.IsZero() || !now.Before(from)) && (until.IsZero() || now.Before(until))
}

// Ended says whether the trigger's period is over at now.
func (t Trigger) Ended(now time.Time) bool {
	_, until, err := t.period()
	return err == nil && !until.IsZero() && !now.Before(until)
}

// Bounds of a run's time.
const (
	DefaultTimeout = 10 * time.Minute
	MaxTimeout     = 2 * time.Hour
)

// limit is how long a run may take.
func (t Trigger) limit() (time.Duration, error) {
	if t.Timeout == "" {
		return DefaultTimeout, nil
	}
	d, err := time.ParseDuration(t.Timeout)
	if err != nil || d < time.Minute || d > MaxTimeout {
		return 0, errors.New(ErrConfig, "the timeout is a duration from 1m to 2h")
	}
	return d, nil
}

// kindChecks check what sets each kind off.
var kindChecks = map[Kind]func(Trigger) error{
	Time: func(t Trigger) error {
		return need(t.When != "", "a time trigger needs a timetable")
	},
	Tool: func(t Trigger) error {
		if err := need(validTool.MatchString(t.Tool), "a tool trigger needs a tool name"); err != nil {
			return err
		}
		return need(len(t.Args) <= MaxInput && (len(t.Args) == 0 || json.Valid(t.Args)), "the tool's arguments are not JSON of at most 64 KiB")
	},
	File: func(t Trigger) error {
		return need(len(t.Path) <= maxShort && filepath.IsAbs(t.Path), "a file trigger needs an absolute path")
	},
	Message: func(t Trigger) error {
		return need(t.Chat != "" && len(t.Chat) <= maxShort && len(t.Match) <= maxShort, "a message trigger needs a chat (and words of at most 512 bytes)")
	},
	Webhook: func(t Trigger) error {
		return need(len(t.Secret) >= minSecret && len(t.Secret) <= maxShort, "a webhook needs a secret of at least 32 characters")
	},
}

// checkKind checks what sets the trigger off, and that no field of
// another kind is set (it would be ignored, and mislead).
func (t Trigger) checkKind() error {
	check, ok := kindChecks[t.kind()]
	if !ok {
		return errors.New(ErrConfig, "kinds are time, tool, file, message and webhook")
	}
	if err := check(t); err != nil {
		return err
	}
	k := t.kind()
	for field, rule := range map[string]struct {
		set bool
		on  []Kind
	}{
		"when":     {t.When != "", []Kind{Time, Tool, File}},
		"catch_up": {t.CatchUp, []Kind{Time}},
		"tool":     {t.Tool != "" || len(t.Args) > 0, []Kind{Tool}},
		"path":     {t.Path != "", []Kind{File}},
		"chat":     {t.Chat != "" || t.Match != "", []Kind{Message}},
		"secret":   {t.Secret != "", []Kind{Webhook}},
	} {
		if rule.set && !slices.Contains(rule.on, k) {
			return errors.New(ErrConfig, field+" does not apply to a "+string(k)+" trigger")
		}
	}
	return nil
}

func need(ok bool, problem string) error {
	if ok {
		return nil
	}
	return errors.New(ErrConfig, problem)
}

// when is the trigger's timetable: its own, or how often it looks.
func (t Trigger) when() (When, error) {
	spec := t.When
	switch t.kind() {
	case Message, Webhook:
		return nil, nil
	case Tool:
		spec = cmpOr(spec, defaultTool)
	case File:
		spec = cmpOr(spec, defaultFile)
	}
	loc, err := zone(t.TimeZone)
	if err != nil {
		return nil, errors.New(ErrConfig, "trigger "+t.Name+": unknown time zone "+t.TimeZone)
	}
	return Parse(spec, loc)
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func zone(name string) (*time.Location, error) {
	if name == "" {
		return time.Local, nil
	}
	return time.LoadLocation(name)
}
