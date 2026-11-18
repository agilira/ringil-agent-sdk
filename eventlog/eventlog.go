// eventlog.go: an agent's events as log records, metadata only
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package eventlog writes an agent's events to a log/slog logger, keeping
// to the metadata: which run, which step, which tool, how long, how many
// tokens, which error code. Never what was said: no prompt, no model
// text, no tool arguments or results, no error messages (they often carry
// paths, credentials or upstream answers).
//
//	cfg.Observers = append(cfg.Observers, eventlog.Slog(logger))
package eventlog

import (
	"context"
	stderrors "errors"
	"log/slog"
	"regexp"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Slog returns an observer that logs every event of a run except the
// streamed text deltas, which are many and carry nothing but text.
// Failures and loops are warnings or errors; the rest is Info, tool
// starts Debug.
func Slog(l *slog.Logger) agent.Observer {
	return func(ev agent.Event) {
		if ev.Kind == agent.EventText {
			return
		}
		level := levelOf(ev)
		ctx := context.Background()
		if !l.Enabled(ctx, level) {
			return
		}
		l.LogAttrs(ctx, level, "agent."+string(ev.Kind), Attrs(ev)...)
	}
}

func levelOf(ev agent.Event) slog.Level {
	switch {
	case ev.Kind == agent.EventFailed:
		return slog.LevelError
	case ev.Kind == agent.EventLoop, ev.Kind == agent.EventContext, ev.Err != nil:
		return slog.LevelWarn
	case ev.Kind == agent.EventToolStart:
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// Attrs are the metadata of an event, for any logger or exporter: the
// same rule, nothing the model or a tool wrote.
func Attrs(ev agent.Event) []slog.Attr {
	a := []slog.Attr{slog.String("run", ev.Run), slog.Int("seq", ev.Seq)}
	if ev.Worker != "" {
		a = append(a, slog.String("worker", ev.Worker), slog.Int("task", ev.Task))
	}
	if ev.Parent != "" {
		a = append(a, slog.String("parent", ev.Parent))
	}
	if ev.Call != nil {
		a = append(a, tool(ev)...)
	}
	if ev.Output != nil {
		a = append(a, slog.Bool("is_error", ev.Output.IsError), slog.Int("result_bytes", len(ev.Output.Content)))
	}
	if ev.Duration > 0 {
		a = append(a, slog.Int64("ms", ev.Duration.Milliseconds()))
	}
	a = append(a, usage(ev)...)
	if ev.Err != nil {
		a = append(a, slog.String("error", code(ev.Err)))
	}
	if what := ours(ev); what != "" {
		a = append(a, slog.String("what", what))
	}
	return a
}

func usage(ev agent.Event) []slog.Attr {
	var a []slog.Attr
	if ev.Stop != "" {
		a = append(a, slog.String("stop", stop(ev.Stop)))
	}
	u := ev.Usage
	if ev.Kind == agent.EventDone && ev.Result != nil {
		u = ev.Result.Usage
		a = append(a, slog.Int("turns", ev.Result.Turns))
	}
	if u != (agent.Usage{}) {
		a = append(a, slog.Int("in", u.Input), slog.Int("out", u.Output), slog.Int("cache_read", u.CacheRead), slog.Int("cache_write", u.CacheWrite))
	}
	return a
}

// ours is an event's Text when the agent wrote it (what compaction left
// out, how many memories, what the loop detector did), never text from
// the model, a tool or the user.
func ours(ev agent.Event) string {
	switch ev.Kind {
	case agent.EventCompacted, agent.EventRecalled, agent.EventLoop:
		return clip(ev.Text)
	}
	return ""
}

// code is the error's code, or its kind for an error without one.
func code(err error) string {
	var e *errors.Error
	switch {
	case stderrors.As(err, &e):
		return string(e.Code)
	case stderrors.Is(err, context.Canceled):
		return "CANCELLED"
	case stderrors.Is(err, context.DeadlineExceeded):
		return "DEADLINE_EXCEEDED"
	}
	return "UNCLASSIFIED"
}

// tool names the tool of a call when the agent has it: a name the model
// invented is model output, and could carry anything. Call IDs come from
// the provider or the agent, but are logged only when plain.
func tool(ev agent.Event) []slog.Attr {
	name := ident(ev.Call.Name)
	if ev.UnknownTool {
		name = "(unknown tool)"
	}
	return []slog.Attr{slog.String("tool", name), slog.String("call", ident(ev.Call.ID))}
}

// stop is a stop reason the agent defines; a provider may pass anything.
func stop(s agent.StopReason) string {
	switch s {
	case agent.StopEnd, agent.StopToolUse, agent.StopMaxTokens, agent.StopFiltered, agent.StopMaxTurns, agent.StopCancelled, agent.StopLoop:
		return string(s)
	}
	return "(other)"
}

var plain = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// ident is s when it is a plain identifier, else a placeholder.
func ident(s string) string {
	if plain.MatchString(s) {
		return s
	}
	return "(not an identifier)"
}

func clip(s string) string {
	const max = 64
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
