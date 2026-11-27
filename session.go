// session.go: conversations that outlive a run, and the stores that keep them
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

// Conversation is the history an agent reads and extends. The loop appends
// every message as soon as it exists (the user input, the model's message
// before any tool runs, each tool result after it runs), so a crash leaves a
// record of what was asked and done.
type Conversation interface {
	// Messages returns the history so far.
	Messages() []Message
	// Append records messages. An error stops the run before any further
	// effect.
	Append(ctx context.Context, msgs ...Message) error
}

// RunGuard is implemented by conversations that admit one run at a time.
// Two runs on one conversation would each take the other's in-flight calls
// for interrupted ones, so the agent refuses to start a second run on a
// conversation that implements RunGuard and is busy. Both built-in
// conversations and the file sessions implement it.
type RunGuard interface {
	// BeginRun marks the conversation busy, or fails with ErrSessionBusy.
	// The returned function ends the run.
	BeginRun() (end func(), err error)
}

// Session is a stored conversation, held exclusively until Close.
type Session interface {
	Conversation
	ID() string
	Close() error
}

// Store opens, lists and deletes sessions. Implementations live in their
// own packages (session/file, session/mem). A resumed session's history
// reaches the model as it is: the host trusts the store it reads.
type Store interface {
	// Open returns the session with id, creating it if absent. It fails
	// with ErrSessionBusy if another holder has it open.
	Open(ctx context.Context, id string) (Session, error)
	List(ctx context.Context) ([]SessionInfo, error)
	Delete(ctx context.Context, id string) error
}

// SessionInfo describes a stored session.
type SessionInfo struct {
	ID      string
	Updated time.Time
	Size    int64
	// Title, Pinned and Profile are the session's Labels, set by the host
	// (LabelStore): zero when none were.
	Title   string
	Pinned  bool
	Profile string
}

// Labels are what a host keeps of a session beside its history, for a
// list of conversations to show without reading them.
type Labels struct {
	// Title is cleaned (CleanTitle) when stored.
	Title string `json:"title,omitempty"`
	// Pinned keeps the session at the top of the list.
	Pinned bool `json:"pinned,omitempty"`
	// Profile is the agent that answered last, by the host's name: 0-64
	// letters, digits, - and _.
	Profile string `json:"profile,omitempty"`
}

// Check validates labels as a store keeps them.
func (l Labels) Check() error {
	if !validProfile.MatchString(l.Profile) {
		return errors.New(ErrSessionInput, "a session's profile is 0-64 letters, digits, - and _")
	}
	return nil
}

var validProfile = regexp.MustCompile(`^[A-Za-z0-9_-]{0,64}$`)

// LabelStore is a Store that keeps Labels for each session, beside its
// history (session/file does).
type LabelStore interface {
	Store
	// Label changes the labels of the session id: change edits the
	// current ones, and the result, cleaned and checked, is written whole.
	// The changes of one store are made one at a time.
	Label(ctx context.Context, id string, change func(*Labels)) error
}

// MaxTitle bounds a session's title, in bytes.
const MaxTitle = 120

// CleanTitle is title as a list shows it: one line, no control
// characters, spaces folded, at most MaxTitle bytes cut at a word where
// it can.
func CleanTitle(title string) string {
	title = strings.ToValidUTF8(title, "")
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, title)
	title = strings.Join(strings.Fields(title), " ")
	if len(title) <= MaxTitle {
		return title
	}
	cut := MaxTitle - len("…")
	for cut > 0 && !utf8.RuneStart(title[cut]) {
		cut--
	}
	if sp := strings.LastIndexByte(title[:cut], ' '); sp > MaxTitle/2 {
		cut = sp
	}
	return strings.TrimSpace(title[:cut]) + "…"
}

// Session error codes.
const (
	// ErrSessionInput: the session ID is not valid.
	ErrSessionInput errors.ErrorCode = "SESSION_INPUT"
	// ErrSessionBusy: another process or run holds the session.
	ErrSessionBusy errors.ErrorCode = "SESSION_BUSY"
	// ErrSessionCorrupt: the stored session is damaged or was altered.
	ErrSessionCorrupt errors.ErrorCode = "SESSION_CORRUPT"
	// ErrSessionIncomplete: with StrictResume, the session ended in the
	// middle of tool calls.
	ErrSessionIncomplete errors.ErrorCode = "SESSION_INCOMPLETE"
	// ErrSessionIO: the store could not read or write.
	ErrSessionIO errors.ErrorCode = "SESSION_IO"
)

var sessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidateSessionID accepts 1-64 letters, digits, '_' and '-'. IDs become
// file names, so nothing that could leave the store's directory is allowed.
func ValidateSessionID(id string) error {
	if !sessionID.MatchString(id) {
		return errors.New(ErrSessionInput, "session ID must be 1-64 letters, digits, _ or -")
	}
	return nil
}

// ValidateHistory checks the structure of a stored history: known roles,
// tool results that answer an earlier call exactly once, and unanswered
// calls only in the last assistant message (an interrupted run).
func ValidateHistory(msgs []Message) error {
	return NewHistoryChecker().Add(msgs...)
}

// HistoryChecker validates a history one batch at a time, so stores can
// refuse a bad Append before writing it instead of discovering at the next
// load that the session can no longer be read.
type HistoryChecker struct {
	open, seen map[string]bool
	count      int
}

// NewHistoryChecker returns a checker for an empty history.
func NewHistoryChecker() *HistoryChecker {
	return &HistoryChecker{open: map[string]bool{}, seen: map[string]bool{}}
}

// Add checks msgs as the continuation of what was added so far. It is all or
// nothing: on error the checker is unchanged.
func (h *HistoryChecker) Add(msgs ...Message) error {
	open := maps.Clone(h.open)
	var added []string
	for i, msg := range msgs {
		for _, call := range msg.Calls {
			if !h.seen[call.ID] {
				added = append(added, call.ID)
			}
		}
		if err := checkMessage(msg, open, h.seen); err != nil {
			for _, id := range added {
				delete(h.seen, id)
			}
			return errors.Wrap(err, ErrSessionCorrupt, "message "+strconv.Itoa(h.count+i))
		}
	}
	h.open, h.count = open, h.count+len(msgs)
	return nil
}

func checkMessage(msg Message, open, seen map[string]bool) error {
	switch msg.Role {
	case RoleUser:
		return checkClosed(open, len(msg.Calls) == 0 && msg.Result == nil)
	case RoleAssistant:
		if err := checkClosed(open, msg.Result == nil); err != nil {
			return err
		}
		return openCalls(msg.Calls, open, seen)
	case RoleSystem:
		return checkClosed(open, plainText(msg))
	case RoleTool:
		return closeCall(msg, open)
	}
	return errors.New(ErrSessionCorrupt, "unknown role")
}

// closeCall takes a tool result: it answers a call still open.
func closeCall(msg Message, open map[string]bool) error {
	if msg.Result == nil || !open[msg.Result.CallID] || len(msg.Calls) != 0 {
		return errors.New(ErrSessionCorrupt, "tool result without a matching call")
	}
	delete(open, msg.Result.CallID)
	return nil
}

// plainText: msg holds text and neither calls nor a result.
func plainText(msg Message) bool {
	return len(msg.Calls) == 0 && msg.Result == nil && strings.TrimSpace(msg.ModelText()) != ""
}

// checkClosed refuses a new turn while calls are still unanswered: only the
// end of a history may hold an interrupted turn.
func checkClosed(open map[string]bool, wellFormed bool) error {
	if len(open) > 0 || !wellFormed {
		return errors.New(ErrSessionCorrupt, "malformed message or unanswered calls before it")
	}
	return nil
}

func openCalls(calls []ToolCall, open, seen map[string]bool) error {
	for _, call := range calls {
		if call.ID == "" || seen[call.ID] {
			return errors.New(ErrSessionCorrupt, "missing or repeated call ID")
		}
		seen[call.ID], open[call.ID] = true, true
	}
	return nil
}

// pendingCalls returns the calls of the last assistant message that have no
// result: the run that made them stopped before recording what happened.
func pendingCalls(msgs []Message) []ToolCall {
	answered := map[string]bool{}
	for i := len(msgs) - 1; i >= 0; i-- {
		switch msgs[i].Role {
		case RoleTool:
			answered[msgs[i].Result.CallID] = true
		case RoleAssistant:
			var pending []ToolCall
			for _, call := range msgs[i].Calls {
				if !answered[call.ID] {
					pending = append(pending, call)
				}
			}
			return pending
		default:
			return nil
		}
	}
	return nil
}

// interruptedResult is what the model is told about a call whose outcome
// was never recorded. The call is never repeated automatically: it may have
// had its effect before the interruption.
const interruptedResult = "Interrupted before a result was recorded: the action may or may not have happened. " +
	"Check its effects before trying it again."

// NewConversation returns an in-memory conversation starting from history.
// It is safe for concurrent use.
func NewConversation(history ...Message) Conversation {
	c := &memoryConversation{checker: NewHistoryChecker()}
	for _, msg := range history {
		c.msgs = append(c.msgs, msg.Clone())
	}
	// A malformed starting history is reported by the first run; later
	// Appends are then checked against an empty history.
	if c.checker.Add(c.msgs...) != nil {
		c.checker = nil
	}
	return c
}

type memoryConversation struct {
	mu      sync.Mutex
	msgs    []Message
	checker *HistoryChecker
	running bool
}

func (c *memoryConversation) BeginRun() (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return nil, errors.New(ErrSessionBusy, "another run is using this conversation")
	}
	c.running = true
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.running = false
	}, nil
}

func (c *memoryConversation) Messages() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, len(c.msgs))
	for i, msg := range c.msgs {
		out[i] = msg.Clone()
	}
	return out
}

func (c *memoryConversation) Append(ctx context.Context, msgs ...Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checker != nil {
		if err := c.checker.Add(msgs...); err != nil {
			return err
		}
	}
	for _, msg := range msgs {
		c.msgs = append(c.msgs, msg.Clone())
	}
	return nil
}
