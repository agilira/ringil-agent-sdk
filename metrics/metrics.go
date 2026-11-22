// metrics.go: what agents did, counted, for Prometheus
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package metrics counts what agents do, from their events: runs, model
// calls and their tokens, tool calls, how long they took. Give
// [Metrics.Observer] to agent.Config.Observers (a delegating agent passes
// its observers to its workers, so their work counts too), and serve
// [Metrics.Handler] where Prometheus scrapes it.
//
// Only metadata is kept, never content: no text, no tool input or
// output. Labels are bounded: a profile, a tool's name (the first
// MaxTools per profile; then "(other)"; a name the model invented is
// "(unknown)"), an outcome. Money is not counted: prices are the host's to
// know, from the tokens.
package metrics

import (
	"context"
	stderrors "errors"
	"regexp"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// MaxTools is how many tool names are counted apart per profile.
const MaxTools = 256

// Buckets of the durations, in seconds.
var (
	modelBuckets = []float64{0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
	toolBuckets  = []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 15, 60, 300}
)

// Metrics are the counts of the agents observed. The zero value is not
// usable: call New.
type Metrics struct {
	start time.Time

	mu       sync.Mutex
	profiles map[string]*counts
}

// counts are one profile's.
type counts struct {
	started, delegated int64
	active             map[string]bool // top-level runs going on
	finished           map[outcome]int64
	modelCalls         int64
	model              histogram
	tokens             agent.Usage
	compactions        int64
	loops              map[string]int64
	tools              map[toolOutcome]int64
	toolNames          map[string]bool
	tool               histogram
}

type outcome struct{ status, reason string }

type toolOutcome struct{ name, status string }

// New returns empty metrics.
func New() *Metrics { return &Metrics{start: time.Now(), profiles: map[string]*counts{}} }

// Observer counts the events of profile's runs ("" for an agent without
// profiles). profile must come from a small fixed set of names (never
// user or chat IDs) so metric series stay bounded. It is safe for
// concurrent use, as observers must be.
func (m *Metrics) Observer(profile string) agent.Observer {
	profile = label(profile)
	return func(ev agent.Event) { m.observe(profile, ev) }
}

func (m *Metrics) observe(profile string, ev agent.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.profiles[profile]
	if c == nil {
		c = newCounts()
		m.profiles[profile] = c
	}
	c.add(ev)
}

func newCounts() *counts {
	return &counts{
		active: map[string]bool{}, finished: map[outcome]int64{}, loops: map[string]int64{},
		tools: map[toolOutcome]int64{}, toolNames: map[string]bool{},
		model: newHistogram(modelBuckets), tool: newHistogram(toolBuckets),
	}
}

func (c *counts) add(ev agent.Event) {
	switch ev.Kind {
	case agent.EventStarted:
		c.begin(ev)
	case agent.EventModel:
		c.modelCalls++
		c.model.observe(ev.Duration.Seconds())
		c.tokens = c.tokens.Add(ev.Usage)
	case agent.EventCompacted:
		c.compactions++
		c.tokens = c.tokens.Add(ev.Usage) // the summary's
	case agent.EventLoop:
		c.loops[loopAction(ev.Text)]++
	case agent.EventToolEnd:
		c.toolEnd(ev)
	case agent.EventDone:
		c.end(ev, outcome{"done", stopReason(ev.Stop)})
	case agent.EventFailed:
		c.end(ev, outcome{"failed", errorClass(ev.Err)})
	}
}

// begin counts a run; a delegated task is counted apart, and is not a
// run the host waits for.
func (c *counts) begin(ev agent.Event) {
	if ev.Parent != "" {
		c.delegated++
		return
	}
	c.started++
	c.active[ev.Run] = true
}

func (c *counts) end(ev agent.Event, o outcome) {
	if ev.Parent != "" {
		return
	}
	delete(c.active, ev.Run)
	c.finished[o]++
}

func (c *counts) toolEnd(ev agent.Event) {
	name := unknownTool
	if ev.Call != nil && !ev.UnknownTool && ev.Call.Name != "" {
		name = c.toolName(label(ev.Call.Name))
	}
	status := "ok"
	if ev.Err != nil || ev.Output != nil && ev.Output.IsError {
		status = "error"
	}
	c.tools[toolOutcome{name, status}]++
	c.tool.observe(ev.Duration.Seconds())
}

// The tool labels that are not a tool: in parentheses, which no tool's
// name has, so a tool called "other" is not mixed with them.
const (
	otherTools  = "(other)"
	unknownTool = "(unknown)"
)

// toolName is name, or (other) past MaxTools names.
func (c *counts) toolName(name string) string {
	if c.toolNames[name] || len(c.toolNames) < MaxTools {
		c.toolNames[name] = true
		return name
	}
	return otherTools
}

// stopReason is one of the agent's stop reasons: a provider of the
// host's may say anything, and labels must stay few.
func stopReason(s agent.StopReason) string {
	switch s {
	case agent.StopEnd, agent.StopToolUse, agent.StopMaxTokens, agent.StopFiltered, agent.StopMaxTurns,
		agent.StopCancelled, agent.StopLoop, agent.StopDenied:
		return string(s)
	}
	return "other"
}

func loopAction(text string) string {
	switch text {
	case "warned", "blocked", "stopped":
		return text
	}
	return "other"
}

var codePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,47}$`)

// errorClass names a failure by its code (PROVIDER_RATE...), never by its
// text, which may carry anything.
func errorClass(err error) string {
	var coded *errors.Error
	switch {
	case err == nil:
		return "unknown"
	case stderrors.Is(err, context.Canceled):
		return "cancelled"
	case stderrors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case stderrors.As(err, &coded) && codePattern.MatchString(string(coded.Code)):
		return string(coded.Code)
	}
	return "other"
}

// label bounds a label value to at most 64 bytes without splitting UTF-8 runes.
func label(s string) string {
	if len(s) <= 64 {
		return s
	}
	n := 0
	for i := range s {
		if i > 64 {
			break
		}
		n = i
	}
	return s[:n]
}

// histogram counts observations in cumulative buckets, as Prometheus.
type histogram struct {
	bounds []float64
	counts []int64 // per bound, cumulative at write
	sum    float64
	n      int64
}

func newHistogram(bounds []float64) histogram {
	return histogram{bounds: bounds, counts: make([]int64, len(bounds))}
}

func (h *histogram) observe(v float64) {
	if v < 0 {
		v = 0
	}
	h.n++
	h.sum += v
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
			return
		}
	}
}
