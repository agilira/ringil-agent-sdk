// loop.go: noticing a model that repeats itself
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strconv"

	"github.com/agilira/go-errors"
)

// LoopAction is what the agent does once a model repeats itself.
type LoopAction int

const (
	// LoopReport runs the calls and reports the loop to the host only (the
	// tool events say "repeated"): the model's results are its tools' own.
	// The default.
	LoopReport LoopAction = iota
	// LoopWarn runs the calls and tells the model, in each repeated
	// result, that it is going round in circles (Loop.Warning): small
	// models need it.
	LoopWarn
	// LoopBlock warns, then refuses to run the next call that would
	// continue the pattern; the model is told why.
	LoopBlock
	// LoopStop warns, then ends the run with StopLoop instead of running
	// the next call that would continue the pattern.
	LoopStop
)

// Loop configures the loop detector. A loop is the same call giving the
// same result again and again: one call, or a cycle of two or three
// (A B A B, A B C A B C). A repeated call whose result changes, such as
// reading the next page, is progress, not a loop.
type Loop struct {
	// Disabled turns the detector off.
	Disabled bool
	// Repeats is how many times a pattern must occur to count as a loop;
	// zero means 3, the least is 2.
	Repeats int
	// Action is what happens then; the zero value reports it to the host.
	Action LoopAction
	// Warning is the text put before a repeated result (LoopWarn and the
	// others), from how many times the pattern repeated and its length in
	// calls; nil is the agent's own.
	Warning func(times, period int) string
	// Exempt names tools that may repeat freely (a status poll): their
	// calls are not counted and do not break a pattern.
	Exempt []string
}

func (l Loop) validate() error {
	// A cycle of three repeated more than this does not fit the window:
	// a larger value would silently turn detection off.
	if l.Repeats < 0 || l.Repeats == 1 || l.Repeats > maxRepeats || l.Action < LoopReport || l.Action > LoopStop {
		return errors.New(ErrAgentConfig, "loop repeats must be 0 (default 3) or 2.."+strconv.Itoa(maxRepeats)+", and the action one of LoopReport, LoopWarn, LoopBlock, LoopStop")
	}
	return nil
}

func (l Loop) repeats() int {
	if l.Repeats == 0 {
		return 3
	}
	return l.Repeats
}

const (
	// loopWindow is how many calls the detector remembers.
	loopWindow = 32
	// loopMaxPeriod is the longest cycle recognized.
	loopMaxPeriod = 3
	// maxRepeats is the most repetitions the window can show of the
	// longest cycle.
	maxRepeats = loopWindow / loopMaxPeriod
)

type loopEntry struct {
	call, out [sha256.Size]byte
}

// loopDetector watches the calls of one run.
type loopDetector struct {
	cfg    Loop
	exempt map[string]bool
	seen   []loopEntry
	// canon gives the meaning of a call's arguments when the registry
	// knows it; nil compares the JSON.
	canon func(ToolCall) ([]byte, bool)
}

// argsCanonicalizer is a registry that can say what a call's arguments
// mean (the default registry, for tools made with NewTool).
type argsCanonicalizer interface {
	canonicalArgs(call ToolCall) ([]byte, bool)
}

func newLoopDetector(cfg Loop, tools Registry) *loopDetector {
	if cfg.Disabled {
		return nil
	}
	d := &loopDetector{cfg: cfg, exempt: map[string]bool{}}
	if c, ok := tools.(argsCanonicalizer); ok {
		d.canon = c.canonicalArgs
	}
	for _, n := range cfg.Exempt {
		d.exempt[n] = true
	}
	return d
}

// pattern returns the shortest period whose repetition ends the window,
// and how many times it occurs; 0, 0 when nothing repeats.
func (d *loopDetector) pattern() (period, times int) {
	for p := 1; p <= loopMaxPeriod; p++ {
		if t := d.times(p); t > times {
			period, times = p, t
		}
	}
	return period, times
}

// times is how many whole times the last p entries repeat at the end of
// the window; 0 when they do not.
func (d *loopDetector) times(p int) int {
	n := len(d.seen)
	run := 0
	for i := n - 1; i-p >= 0 && d.seen[i] == d.seen[i-p]; i-- {
		run++
	}
	if p >= n || run < p {
		return 0
	}
	return run/p + 1
}

// continues reports whether call would extend a loop already detected,
// of any period: the check made before a call runs, for LoopBlock and
// LoopStop.
func (d *loopDetector) continues(call ToolCall) bool {
	if d == nil || d.exempt[call.Name] {
		return false
	}
	h := d.hash(call)
	for p := 1; p <= loopMaxPeriod; p++ {
		if d.times(p) >= d.cfg.repeats() && d.seen[len(d.seen)-p].call == h {
			return true
		}
	}
	return false
}

// clone is a copy for deciding a batch of calls run together.
func (d *loopDetector) clone() *loopDetector {
	if d == nil {
		return nil
	}
	c := *d
	c.seen = append([]loopEntry(nil), d.seen...)
	return &c
}

// admit records a call allowed to run in this batch, whose result is not
// known yet: it is expected to give what it gave last time, or the same
// as the other calls just like it. Calls run together are then checked
// against each other, not only against the past.
func (d *loopDetector) admit(call ToolCall) {
	if d == nil || d.exempt[call.Name] {
		return
	}
	h := d.hash(call)
	out := sha256.Sum256(append([]byte("pending\x00"), h[:]...))
	for i := len(d.seen) - 1; i >= 0; i-- {
		if d.seen[i].call == h {
			out = d.seen[i].out
			break
		}
	}
	d.push(loopEntry{call: h, out: out})
}

func (d *loopDetector) push(e loopEntry) {
	if len(d.seen) == loopWindow {
		d.seen = append(d.seen[:0], d.seen[1:]...)
	}
	d.seen = append(d.seen, e)
}

// add records a call and its result; it returns how many times the
// pattern it ends has occurred, when that is a loop, and its period.
func (d *loopDetector) add(call ToolCall, res ToolResult) (times, period int) {
	if d == nil || d.exempt[call.Name] {
		return 0, 0
	}
	d.push(loopEntry{call: d.hash(call), out: resultHash(res)})
	p, t := d.pattern()
	if t < d.cfg.repeats() {
		return 0, 0
	}
	return t, p
}

// hash identifies a call by its tool and what its arguments mean: as the
// tool decodes them when it can say (so {"a":1,"b":0} with b defaulting
// to 0 is {"a":1}, and an ignored field is no difference), else as JSON
// whatever its key order or spacing.
func (d *loopDetector) hash(call ToolCall) [sha256.Size]byte {
	if d.canon != nil {
		if args, ok := d.canon(call); ok {
			return sha256.Sum256(append(append([]byte(call.Name), 0), args...))
		}
	}
	args := []byte(call.Input)
	var v any
	dec := json.NewDecoder(bytes.NewReader(call.Input))
	dec.UseNumber()
	if dec.Decode(&v) == nil {
		if canonical, err := json.Marshal(v); err == nil {
			args = canonical
		}
	}
	return sha256.Sum256(append(append([]byte(call.Name), 0), args...))
}

func resultHash(res ToolResult) [sha256.Size]byte {
	flag := byte(0)
	if res.IsError {
		flag = 1
	}
	return sha256.Sum256(append([]byte{flag}, res.Content...))
}

// loopWarning goes before a result that repeats: at the start it
// survives compaction, which keeps the beginning of a long result.
func loopWarning(times, period int) string {
	what := "this same call"
	if period > 1 {
		what = "this same cycle of " + strconv.Itoa(period) + " calls"
	}
	return "[Loop: " + what + " has given the same result " + strconv.Itoa(times) +
		" times. Repeating it will not change the outcome: change approach, or answer with what you have.]\n"
}

// loopRefusal answers a call LoopBlock did not run.
const loopRefusal = "Not run: this call would repeat a loop of calls that keep giving the same result. Change approach, or answer with what you have."
