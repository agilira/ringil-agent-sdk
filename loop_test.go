// loop_test.go: the loop detector
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// constant is a tool that always gives the same answer and counts its runs.
func constant(name string, runs *atomic.Int32) agent.Tool {
	type in struct {
		A int `json:"a,omitempty"`
		B int `json:"b,omitempty"`
	}
	return agent.NewTool(name, "always the same", func(context.Context, in) (string, error) {
		runs.Add(1)
		return name + " result", nil
	})
}

// script calls each name in order, then answers "done".
func script(names ...string) []fake.Step {
	steps := make([]fake.Step, 0, len(names)+1)
	for _, n := range names {
		steps = append(steps, fake.Call(n, `{}`))
	}
	return append(steps, fake.Reply("done"))
}

// loopRun runs the script and returns the tool results and loop events.
func loopRun(t *testing.T, cfg agent.Config, steps []fake.Step) (results, loops []string, res *agent.Result) {
	t.Helper()
	cfg.Provider = fake.New(steps...)
	a := newAgent(t, cfg)
	for ev, err := range a.Stream(context.Background(), nil, agent.UserText("go")) {
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case agent.EventToolEnd:
			results = append(results, ev.Output.Content)
		case agent.EventLoop:
			loops = append(loops, ev.Call.Name+":"+ev.Text)
		case agent.EventDone:
			res = ev.Result
		}
	}
	return results, loops, res
}

var warn = agent.Loop{Action: agent.LoopWarn}

func warned(results []string) string {
	var marks []string
	for _, r := range results {
		marks = append(marks, strconv.FormatBool(strings.Contains(r, "[Loop:")))
	}
	return strings.Join(marks, " ")
}

// By default a loop is the host's to know: the results stay the tools'
// own (21/11: the model's results were written into by default).
func TestLoopIsReportedByDefault(t *testing.T) {
	var runs atomic.Int32
	results, loops, _ := loopRun(t, agent.Config{Tools: tools(t, constant("ping", &runs))}, script("ping", "ping", "ping", "ping"))
	if got := warned(results); got != "false false false false" || len(loops) != 2 || loops[0] != "ping:repeated" {
		t.Fatalf("warned %s, %v", got, loops)
	}
}

func TestLoopWarns(t *testing.T) {
	var runs atomic.Int32
	results, loops, res := loopRun(t, agent.Config{Tools: tools(t, constant("ping", &runs)), Loop: warn}, script("ping", "ping", "ping", "ping"))
	if got := warned(results); got != "false false true true" || runs.Load() != 4 || res.Text != "done" {
		t.Fatalf("warned %s runs %d text %q", got, runs.Load(), res.Text)
	}
	if !strings.Contains(results[2], "this same call has given the same result 3 times") || len(loops) != 2 || loops[0] != "ping:warned" {
		t.Fatalf("%q %v", results[2], loops)
	}
}

// A repeated call whose result changes is progress (the next page).
func TestChangingResultsAreNotALoop(t *testing.T) {
	page := 0
	next := agent.NewTool("read_page", "next page", func(context.Context, struct{}) (string, error) {
		page++
		return "page " + strconv.Itoa(page), nil
	})
	results, loops, _ := loopRun(t, agent.Config{Tools: tools(t, next), Loop: warn}, script("read_page", "read_page", "read_page", "read_page", "read_page"))
	if got := warned(results); strings.Contains(got, "true") || len(loops) != 0 {
		t.Fatalf("warned %s", got)
	}
}

// The defect of 15/11: alternating calls and cycles of three went unseen.
func TestCyclesOfTwoAndThree(t *testing.T) {
	var runs atomic.Int32
	reg := tools(t, constant("a", &runs), constant("b", &runs), constant("c", &runs))
	results, _, _ := loopRun(t, agent.Config{Tools: reg, Loop: warn}, script("a", "b", "a", "b", "a", "b"))
	// Three whole cycles make a loop: the sixth call.
	if got := warned(results); got != "false false false false false true" || !strings.Contains(results[5], "cycle of 2 calls") {
		t.Fatalf("cycle of 2: %s %q", got, results[5])
	}
	results, _, _ = loopRun(t, agent.Config{Tools: reg, Loop: warn}, script("a", "b", "c", "a", "b", "c", "a", "b", "c"))
	if got := warned(results); got != "false false false false false false false false true" || !strings.Contains(results[8], "cycle of 3 calls") {
		t.Fatalf("cycle of 3: %s", got)
	}
}

// The same arguments in another order or spacing are the same call.
func TestArgumentOrderDoesNotHideALoop(t *testing.T) {
	var runs atomic.Int32
	steps := []fake.Step{fake.Call("ping", `{"a":1,"b":2}`), fake.Call("ping", `{"b":2, "a":1}`), fake.Call("ping", `{ "a": 1, "b": 2 }`), fake.Reply("done")}
	results, _, _ := loopRun(t, agent.Config{Tools: tools(t, constant("ping", &runs)), Loop: warn}, steps)
	if got := warned(results); got != "false false true" {
		t.Fatalf("warned %s", got)
	}
}

func TestLoopBlockRefusesTheNextRepeat(t *testing.T) {
	var runs atomic.Int32
	cfg := agent.Config{Tools: tools(t, constant("ping", &runs)), Loop: agent.Loop{Action: agent.LoopBlock}}
	results, loops, res := loopRun(t, cfg, script("ping", "ping", "ping", "ping", "ping"))
	if runs.Load() != 3 || res.Text != "done" || !strings.HasPrefix(results[3], "Not run: this call would repeat a loop") {
		t.Fatalf("runs %d %q %q", runs.Load(), results[3], res.Text)
	}
	if strings.Join(loops, ",") != "ping:warned,ping:blocked,ping:blocked" {
		t.Fatalf("loops %v", loops)
	}
}

// Approval is not asked for a call the detector refuses.
func TestLoopBlockBeforeApproval(t *testing.T) {
	var runs, asked atomic.Int32
	cfg := agent.Config{Tools: tools(t, constant("ping", &runs)), Loop: agent.Loop{Action: agent.LoopBlock},
		Approve: func(context.Context, agent.ToolCall, agent.ToolSpec) (agent.Decision, error) {
			asked.Add(1)
			return agent.Allow(), nil
		}}
	loopRun(t, cfg, script("ping", "ping", "ping", "ping"))
	if asked.Load() != 3 {
		t.Fatalf("asked %d times", asked.Load())
	}
}

func TestLoopStopEndsTheRun(t *testing.T) {
	var runs atomic.Int32
	cfg := agent.Config{Tools: tools(t, constant("ping", &runs)), Loop: agent.Loop{Action: agent.LoopStop, Repeats: 2}}
	results, loops, res := loopRun(t, cfg, script("ping", "ping", "ping", "ping"))
	if runs.Load() != 2 || res.Stop != agent.StopLoop || len(results) != 3 || strings.Join(loops, ",") != "ping:warned,ping:stopped" {
		t.Fatalf("runs %d stop %s results %d loops %v", runs.Load(), res.Stop, len(results), loops)
	}
}

func TestExemptAndDisabled(t *testing.T) {
	var runs atomic.Int32
	reg := tools(t, constant("ping", &runs))
	for _, l := range []agent.Loop{{Exempt: []string{"ping"}, Action: agent.LoopStop}, {Disabled: true}} {
		results, loops, res := loopRun(t, agent.Config{Tools: reg, Loop: l}, script("ping", "ping", "ping", "ping", "ping"))
		if got := warned(results); strings.Contains(got, "true") || len(loops) != 0 || res.Stop != agent.StopEnd {
			t.Fatalf("%+v: warned %s loops %v", l, got, loops)
		}
	}
}

func TestLoopConfigValidated(t *testing.T) {
	for _, l := range []agent.Loop{{Repeats: 1}, {Repeats: -1}, {Action: 7}} {
		if _, err := agent.New(agent.Config{Provider: fake.New(), Loop: l}); !errors.HasCode(err, agent.ErrAgentConfig) {
			t.Errorf("%+v accepted: %v", l, err)
		}
	}
}

func roConst(name string, runs *atomic.Int32) agent.Tool {
	return agent.NewTool(name, "same", func(context.Context, struct{}) (string, error) {
		runs.Add(1)
		return name + " result", nil
	}, agent.ReadOnly())
}

func together(names ...string) fake.Step {
	calls := make([]agent.ToolCall, len(names))
	for i, n := range names {
		calls[i] = agent.ToolCall{Name: n, Input: json.RawMessage(`{}`)}
	}
	return fake.Step{Calls: calls}
}

// Audit 18/11 (1): ten identical read-only calls in one turn all ran
// under LoopStop; calls run together are checked against each other.
func TestLoopInsideOneBatch(t *testing.T) {
	var runs atomic.Int32
	names := strings.Fields(strings.Repeat("ping ", 10))
	cfg := agent.Config{Tools: tools(t, roConst("ping", &runs)), Loop: agent.Loop{Action: agent.LoopStop, Repeats: 2}}
	results, _, res := loopRun(t, cfg, []fake.Step{together(names...), fake.Reply("done")})
	if runs.Load() != 2 || res.Stop != agent.StopLoop || len(results) != 10 {
		t.Fatalf("runs %d stop %s results %d", runs.Load(), res.Stop, len(results))
	}
}

// Audit 18/11 (3): only the most repeated period was checked; a shorter
// loop at the end (a a) is refused too.
func TestShorterPeriodIsChecked(t *testing.T) {
	var runs atomic.Int32
	reg := tools(t, roConst("a", &runs), roConst("b", &runs))
	cfg := agent.Config{Tools: reg, Loop: agent.Loop{Action: agent.LoopBlock, Repeats: 2}}
	steps := []fake.Step{together("b", "a", "a", "b", "a", "a", "b", "a", "a"), fake.Call("a", `{}`), fake.Reply("done")}
	results, _, _ := loopRun(t, cfg, steps)
	if last := results[len(results)-1]; !strings.HasPrefix(last, "Not run") {
		t.Fatalf("the trailing a a loop was not refused: %q", last)
	}
}

// Audit 18/11 (2): the warning was appended and compaction, which keeps
// a long result's beginning, cut it off. It now comes first.
func TestWarningSurvivesCompaction(t *testing.T) {
	big := agent.NewTool("read", "big", func(context.Context, struct{}) (string, error) {
		return strings.Repeat("x", 20000), nil
	}, agent.ReadOnly())
	p := fake.New(fake.Call("read", `{}`), fake.Call("read", `{}`), together("read", "read"), fake.Reply("done"))
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, big), ContextWindow: 4000, Loop: warn})
	if _, err := a.Run(context.Background(), nil, agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	last := p.Requests()[3].Messages
	if c := last[len(last)-1].Result.Content; !strings.HasPrefix(c, "[Loop:") {
		t.Fatalf("warning lost: %.100q", c)
	}
}

// Audit 18/11 (4): Repeats too large for the window turned detection off
// silently; it is refused.
func TestRepeatsBeyondTheWindowRefused(t *testing.T) {
	if _, err := agent.New(agent.Config{Provider: fake.New(), Loop: agent.Loop{Repeats: 11}}); !errors.HasCode(err, agent.ErrAgentConfig) {
		t.Fatalf("accepted: %v", err)
	}
	if _, err := agent.New(agent.Config{Provider: fake.New(), Loop: agent.Loop{Repeats: 10}}); err != nil {
		t.Fatal(err)
	}
}

// Audit 18/11 (5): the same meaning written differently ran past
// LoopBlock. Arguments are compared as the tool decodes them: a default
// written out or a null is no difference. (An unknown field is dropped
// too, but its result carries a note that it was ignored, so the result
// differs.)
func TestSameMeaningIsTheSameCall(t *testing.T) {
	var runs atomic.Int32
	steps := []fake.Step{fake.Call("ping", `{"a":1}`), fake.Call("ping", `{"a":1,"b":0}`), fake.Call("ping", `{"b":null,"a":1}`), fake.Call("ping", `{ "a" : 1 }`), fake.Reply("done")}
	cfg := agent.Config{Tools: tools(t, constant("ping", &runs)), Loop: agent.Loop{Action: agent.LoopBlock}}
	results, _, _ := loopRun(t, cfg, steps)
	if runs.Load() != 3 || !strings.HasPrefix(results[3], "Not run") {
		t.Fatalf("runs %d, last %q", runs.Load(), results[3])
	}
	// A different value is a different call.
	runs.Store(0)
	steps = []fake.Step{fake.Call("ping", `{"a":1}`), fake.Call("ping", `{"a":1}`), fake.Call("ping", `{"a":2}`), fake.Reply("done")}
	results, _, _ = loopRun(t, cfg, steps)
	if runs.Load() != 3 || strings.Contains(warned(results), "true") {
		t.Fatalf("runs %d warned %s", runs.Load(), warned(results))
	}
}

// The warning is the host's to word.
func TestLoopWarningWording(t *testing.T) {
	var runs atomic.Int32
	l := agent.Loop{Action: agent.LoopWarn, Warning: func(times, period int) string { return "[Loop: giro " + strconv.Itoa(times) + "]\n" }}
	results, _, _ := loopRun(t, agent.Config{Tools: tools(t, constant("ping", &runs)), Loop: l}, script("ping", "ping", "ping"))
	if !strings.HasPrefix(results[2], "[Loop: giro 3]") {
		t.Fatalf("%q", results[2])
	}
}
