// later.go: checks the agent comes back to during a conversation
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package later lets the agent come back to something while a
// conversation goes on: "check the download every 5 minutes", "look at
// the build again in 10". Each check is a turn of the same conversation,
// started by the host when it is due, so the agent sees what it said and
// the user's usual approvals hold. Checks are temporary and bounded (how
// often, how many, how long) and live in memory: they end with their
// limits, when the agent stops them, or when the host stops. What lasts
// beyond the conversation is a trigger, not a check.
package later

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Error codes.
const (
	ErrLimit   errors.ErrorCode = "LATER_LIMIT"
	ErrUnknown errors.ErrorCode = "LATER_UNKNOWN"
	ErrInput   errors.ErrorCode = "LATER_INPUT"
)

// fail is a refusal the model reads (it can put its call right).
func fail(code errors.ErrorCode, message string) error {
	return agent.ToolError(code, message)
}

// Quiet is what a check answers when it found nothing worth telling: the
// host shows nothing.
const Quiet = "[quiet]"

// IsQuiet reports whether a check's answer is Quiet.
func IsQuiet(answer string) bool { return strings.TrimSpace(answer) == Quiet }

// Limits bound the checks of a Board. Zero fields take the defaults.
type Limits struct {
	MinEvery  time.Duration // shortest wait between checks (1m)
	MaxSpan   time.Duration // longest a check may go on (24h)
	MaxChecks int           // most times one check runs (48)
	MaxOpen   int           // most checks open in one conversation (3)
}

func (l Limits) withDefaults() Limits {
	if l.MinEvery <= 0 {
		l.MinEvery = time.Minute
	}
	if l.MaxSpan <= 0 {
		l.MaxSpan = 24 * time.Hour
	}
	if l.MaxChecks <= 0 {
		l.MaxChecks = 48
	}
	if l.MaxOpen <= 0 {
		l.MaxOpen = 3
	}
	return l
}

// MaxWhat bounds what a check is told to look at.
const MaxWhat = 2000

// Check is one thing the agent comes back to.
type Check struct {
	ID           int
	Conversation string // the host's key for the conversation it belongs to
	What         string // what to look at and what to do
	Every        time.Duration
	Until        time.Time
	Max          int
	Done         int  // checks run, this one included when it is handed out
	Last         bool // this is the last one: the check then ends
}

// Board holds the checks of a host, for every conversation. due hears of
// each check when it comes due, in a goroutine of its own, and says
// whether the host took it (started its turn or queued it); one not taken
// (the conversation is busy) is offered again shortly, not counted.
type Board struct {
	mu     sync.Mutex
	limits Limits
	due    func(Check) bool
	next   int
	checks map[int]*entry
	closed bool
}

type entry struct {
	check Check
	timer *time.Timer
}

// NewBoard returns an empty Board.
func NewBoard(l Limits, due func(Check) bool) *Board {
	return &Board{limits: l.withDefaults(), due: due, checks: map[int]*entry{}}
}

// Limits returns the bounds in force, defaults filled.
func (b *Board) Limits() Limits { return b.limits }

// Add sets a check in conversation conv: what to look at, every how
// long, for how long (span), at most max times (0: as many as fit in the
// span, within the limits).
func (b *Board) Add(conv, what string, every, span time.Duration, max int) (Check, error) {
	what = strings.TrimSpace(what)
	if err := b.checkAdd(conv, what, every, span, max); err != nil {
		return Check{}, err
	}
	if max == 0 {
		max = min(int(span/every), b.limits.MaxChecks)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return Check{}, fail(ErrLimit, "checks have stopped")
	}
	if len(b.open(conv)) >= b.limits.MaxOpen {
		return Check{}, fail(ErrLimit, "at most "+strconv.Itoa(b.limits.MaxOpen)+" checks may be open in a conversation: stop one first")
	}
	b.next++
	c := Check{ID: b.next, Conversation: conv, What: what, Every: every, Until: time.Now().Add(span), Max: max}
	e := &entry{check: c}
	b.checks[c.ID] = e
	e.timer = time.AfterFunc(every, func() { b.fire(c.ID) })
	return c, nil
}

func (b *Board) checkAdd(conv, what string, every, span time.Duration, max int) error {
	l := b.limits
	switch {
	case conv == "":
		return fail(ErrInput, "no conversation to come back to")
	case what == "":
		return fail(ErrInput, "say what to check")
	case len(what) > MaxWhat:
		return fail(ErrInput, "what to check is at most "+strconv.Itoa(MaxWhat)+" bytes")
	case every < l.MinEvery:
		return fail(ErrLimit, "checks are at least "+l.MinEvery.String()+" apart")
	case span > l.MaxSpan:
		return fail(ErrLimit, "a check goes on for at most "+l.MaxSpan.String())
	case span < every:
		return fail(ErrInput, "the span is shorter than the wait: nothing would run")
	case max < 0 || max > l.MaxChecks:
		return fail(ErrLimit, "a check runs at most "+strconv.Itoa(l.MaxChecks)+" times")
	}
	return nil
}

// open lists conv's checks; b.mu is held.
func (b *Board) open(conv string) []Check {
	var list []Check
	for _, e := range b.checks {
		if e.check.Conversation == conv {
			list = append(list, e.check)
		}
	}
	return list
}

// retryWait is how soon a check the host did not take is offered again.
const retryWait = 30 * time.Second

func (b *Board) fire(id int) {
	b.mu.Lock()
	e := b.checks[id]
	if e == nil || b.closed {
		b.mu.Unlock()
		return
	}
	c := e.check
	c.Done++
	c.Last = c.Done >= c.Max || !time.Now().Add(c.Every).Before(c.Until)
	b.mu.Unlock()
	b.settle(e, c, b.due(c))
}

// settle sets what follows a check handed out (taken) or not.
func (b *Board) settle(e *entry, c Check, taken bool) {
	id := c.ID
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.checks[id] != e || b.closed { // stopped meanwhile
		return
	}
	switch {
	case !taken && time.Now().Before(c.Until):
		e.timer = time.AfterFunc(min(retryWait, c.Every), func() { b.fire(id) })
	case !taken || c.Last:
		delete(b.checks, id)
	default:
		e.check.Done = c.Done
		e.timer = time.AfterFunc(c.Every, func() { b.fire(id) })
	}
}

// Stop ends check id of conversation conv.
func (b *Board) Stop(conv string, id int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.checks[id]
	if e == nil || e.check.Conversation != conv {
		return fail(ErrUnknown, "no check "+strconv.Itoa(id)+" is open here")
	}
	e.timer.Stop()
	delete(b.checks, id)
	return nil
}

// StopAll ends every check of conversation conv (its history cleared).
func (b *Board) StopAll(conv string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, e := range b.checks {
		if e.check.Conversation == conv {
			e.timer.Stop()
			delete(b.checks, id)
		}
	}
}

// Of lists the checks open in conversation conv, oldest first.
func (b *Board) Of(conv string) []Check {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.open(conv)
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].ID < list[j-1].ID; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	return list
}

// Pending reports how many checks are open in conversation conv.
func (b *Board) Pending(conv string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.open(conv))
}

// Close stops every check; due is not called again once it returns
// (a call under way finishes).
func (b *Board) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for id, e := range b.checks {
		e.timer.Stop()
		delete(b.checks, id)
	}
}

// Notice is the message that starts a check's turn.
func Notice(c Check) agent.Message {
	id := strconv.Itoa(c.ID)
	var b strings.Builder
	b.WriteString("<check-later id=\"" + id + "\" n=\"" + strconv.Itoa(c.Done) + "\" of=\"" + strconv.Itoa(c.Max) + "\">\n")
	b.WriteString("This is check " + strconv.Itoa(c.Done) + " of " + strconv.Itoa(c.Max) + " you set (id " + id + "), not a message from the user. Look now at:\n")
	b.WriteString(closeTag.ReplaceAllString(c.What, "</check_later>") + "\n")
	b.WriteString("If it is done or no longer needed, call " + StopTool + " with id " + id + " and tell the user how it went. ")
	if c.Last {
		b.WriteString("This is the last check: tell the user where things stand.\n")
	} else {
		b.WriteString("If nothing changed that the user needs to know, answer exactly " + Quiet + " and nothing else.\n")
	}
	b.WriteString("</check-later>")
	return agent.UserText(b.String())
}

var closeTag = regexp.MustCompile(`(?i)<\s*/\s*check-later\s*>`)
