// event.go: what a running agent reports
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import "time"

// Observer receives every event of a run, in order, whether the caller
// uses Run or Stream: for logging, metrics, tracing, a UI. It is called
// synchronously, before the event reaches the stream, so it must be quick
// and must not keep the Event's pointers past the call.
//
// The order holds within one run. One observer given to several runs at
// once (concurrent conversations, delegated workers running in parallel
// or in the background) is called from several goroutines: it must be
// safe for concurrent use, and tell runs apart by Event.Run. An observer
// that panics is recovered and given no more events of that run.
type Observer func(Event)

// EventKind identifies an [Event].
type EventKind string

// Event kinds, in the order they occur within a turn.
const (
	// EventStarted opens every run: Run is its ID.
	EventStarted EventKind = "started"
	// EventRecovered reports a call of an interrupted earlier run that was
	// answered as interrupted, not executed again.
	EventRecovered EventKind = "recovered"
	// EventText carries a streamed text delta.
	EventText EventKind = "text"
	// EventRecalled reports the memories added to this run's context;
	// Text says how many, Err is set when the memory could not be read.
	EventRecalled EventKind = "recalled"
	// EventContext reports that Config.Context failed (Err); the run goes
	// on without that context.
	EventContext EventKind = "context"
	// EventCompacted precedes a model call that was sent a shortened view
	// of the conversation; Text says what was left out, Usage what a
	// summary cost.
	EventCompacted EventKind = "compacted"
	// EventModel follows every model call, with its usage.
	EventModel EventKind = "model"
	// EventToolStart precedes a tool call.
	EventToolStart EventKind = "tool_start"
	// EventLoop reports a model repeating itself: Call is the call, Text
	// what was done ("warned", "blocked", "stopped").
	EventLoop EventKind = "loop"
	// EventToolEnd follows a tool call, with the result the model will see
	// and, if it failed, the full error for the host.
	EventToolEnd EventKind = "tool_end"
	// EventDone ends a successful run and carries its Result.
	EventDone EventKind = "done"
	// EventFailed ends a run that failed; Err is why. Stream yields it
	// together with the error.
	EventFailed EventKind = "failed"
)

// Event is one step of a run. Events carry content (text, tool input and
// results): they belong to the caller. Logging adapters must keep to the
// metadata (package eventlog does).
type Event struct {
	Kind EventKind
	// Run identifies the run; Seq numbers its events from 1, in the one
	// order every observer and the stream see; Time is when it happened.
	Run  string
	Seq  int
	Time time.Time
	// Duration is how long the model call (EventModel: from the request,
	// compaction of its view and a retry for an overlong context included,
	// to the end of the answer, streamed text included) or the tool call
	// (EventToolEnd) took.
	Duration time.Duration
	// Parent is the run whose tool started this one (a delegated task),
	// "" for a top-level run. Worker and Task are set on the events of a
	// delegated task (package delegate): which worker ran it, and the
	// task's number.
	Parent string
	Worker string
	Task   int
	// UnknownTool is set on the tool events of a call to a name the agent
	// does not have: the name is the model's invention.
	UnknownTool bool
	Text        string
	Call        *ToolCall
	// Output is the tool result the model will see, on EventToolEnd.
	Output *ToolResult
	// Err is the host-side error of a failed tool call. The model saw only
	// Output.Content.
	Err   error
	Usage Usage
	Stop  StopReason
	// Result is set on EventDone.
	Result *Result
}
