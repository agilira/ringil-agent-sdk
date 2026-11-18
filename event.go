// event.go: what a running agent reports
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

// EventKind identifies an [Event].
type EventKind string

// Event kinds, in the order they occur within a turn.
const (
	// EventRecovered reports a call of an interrupted earlier run that was
	// answered as interrupted, not executed again.
	EventRecovered EventKind = "recovered"
	// EventText carries a streamed text delta.
	EventText EventKind = "text"
	// EventRecalled reports the memories added to this run's context;
	// Text says how many, Err is set when the memory could not be read.
	EventRecalled EventKind = "recalled"
	// EventCompacted precedes a model call that was sent a shortened view
	// of the conversation; Text says what was left out, Usage what a
	// summary cost.
	EventCompacted EventKind = "compacted"
	// EventModel follows every model call, with its usage.
	EventModel EventKind = "model"
	// EventToolStart precedes a tool call.
	EventToolStart EventKind = "tool_start"
	// EventToolEnd follows a tool call, with the result the model will see
	// and, if it failed, the full error for the host.
	EventToolEnd EventKind = "tool_end"
	// EventDone ends a successful run and carries its Result.
	EventDone EventKind = "done"
)

// Event is one step of a run. Events carry content (text, tool input and
// results): they belong to the caller. Logging adapters must keep to the
// metadata.
type Event struct {
	Kind EventKind
	Text string
	Call *ToolCall
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
