// hooks.go: the host's functions at fixed points of a run
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/agilira/go-errors"
)

// ErrAgentHook: a hook stopped the run (BeforeRun or BeforeModel returned
// an error), or failed (it panicked). The hook's error is wrapped.
const ErrAgentHook errors.ErrorCode = "AGENT_HOOK"

// Hooks are the host's functions at fixed points of a run, called in the
// run's goroutine one at a time (BeforeTool for each call of a turn before
// any runs, AfterTool for each once all of them ran). Every field is
// optional. A hook that panics fails what it guards, never the process:
// the run (BeforeRun, BeforeModel), the call (BeforeTool, refused), the
// result (AfterTool, withheld).
//
// Hooks change or stop what happens; to watch, Observers see every event.
type Hooks struct {
	// BeforeRun sees the user's input before anything is recorded and
	// returns the input the run records and answers (as is, or changed: a
	// note added, a word redacted). An error ends the run before it
	// starts, with ErrAgentHook: nothing is recorded.
	BeforeRun func(ctx context.Context, input Message) (Message, error)
	// BeforeModel is called before each model call with what is about to
	// be sent (read it, do not change it: its slices are the run's). An
	// error ends the run with ErrAgentHook; what was recorded stays.
	BeforeModel func(ctx context.Context, call ModelCall) error
	// BeforeTool is called for each call before Approve, and returns the
	// call to approve and run: as is, or with other Input (only Input
	// counts; the model's message keeps its own). An error refuses the
	// call: the model is told ModelMessage(err), and the run goes on.
	// Approve then decides on the call as the hook returned it.
	BeforeTool func(ctx context.Context, call ToolCall, spec ToolSpec) (ToolCall, error)
	// AfterTool sees every result before the model does, also when the
	// run was stopped meanwhile (the result is recorded), and returns the
	// one the model gets (to redact it, shorten it, add to it). Its CallID
	// is kept whatever it returns; its images pass the tools' check.
	AfterTool func(ctx context.Context, call ToolCall, result ToolResult) ToolResult
	// AfterRun is told how the run ended, once: its Result, or the error
	// it failed with (res nil). It is called before the run's last event
	// (EventDone or EventFailed) goes out, so that what it does is done
	// when the caller sees the end. It cannot change the outcome.
	AfterRun func(ctx context.Context, res *Result, err error)
}

// ModelCall is what BeforeModel sees of a model call.
type ModelCall struct {
	// Turn is the model calls this run made before this one.
	Turn int
	// Usage is what the run has used so far.
	Usage Usage
	// Request is the request, before compaction.
	Request *Request
}

// beforeRun runs Hooks.BeforeRun; a panic is an error.
func (h Hooks) beforeRun(ctx context.Context, input Message) (out Message, err error) {
	if h.BeforeRun == nil {
		return input, nil
	}
	defer func() {
		if recover() != nil {
			err = errors.New(ErrAgentHook, "the BeforeRun hook panicked")
		}
	}()
	out, err = h.BeforeRun(ctx, input)
	if err != nil {
		return Message{}, errors.Wrap(err, ErrAgentHook, "the BeforeRun hook stopped the run")
	}
	if out.Role != RoleUser || len(out.Calls) > 0 || out.Result != nil {
		return Message{}, errors.New(ErrAgentHook, "the BeforeRun hook returned a message that is not a user message")
	}
	return out, nil
}

// beforeModel runs Hooks.BeforeModel; a panic is an error.
func (h Hooks) beforeModel(ctx context.Context, call ModelCall) (err error) {
	if h.BeforeModel == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			err = errors.New(ErrAgentHook, "the BeforeModel hook panicked")
		}
	}()
	if err := h.BeforeModel(ctx, call); err != nil {
		return errors.Wrap(err, ErrAgentHook, "the BeforeModel hook stopped the run")
	}
	return nil
}

// beforeTool runs Hooks.BeforeTool; a panic refuses the call.
func (h Hooks) beforeTool(ctx context.Context, call ToolCall, spec ToolSpec) (out ToolCall, err error) {
	if h.BeforeTool == nil {
		return call, nil
	}
	defer func() {
		if recover() != nil {
			err = ToolError(ErrAgentHook, "The call was not run: the application failed to check it.")
		}
	}()
	out, err = h.BeforeTool(ctx, call, spec)
	if err == nil && !slices.Equal(out.Input, call.Input) && !json.Valid(out.Input) {
		return ToolCall{}, ToolError(ErrAgentHook, "The call was not run: the application produced invalid arguments.")
	}
	return out, err
}

// afterTool runs Hooks.AfterTool; ok is false when it panicked.
func (h Hooks) afterTool(ctx context.Context, call ToolCall, res ToolResult) (out ToolResult, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return h.AfterTool(ctx, call, res), true
}

// afterRun runs Hooks.AfterRun; a panic in it is swallowed: the run has
// ended either way.
func (h Hooks) afterRun(ctx context.Context, res *Result, err error) {
	if h.AfterRun == nil {
		return
	}
	defer func() { _ = recover() }()
	h.AfterRun(ctx, res, err)
}
