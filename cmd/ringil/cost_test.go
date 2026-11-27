// cost_test.go: prices, dollar limits and the cost shown
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/agilira/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/price"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// testPrices is a list in OpenRouter's format, as the catalog gives it.
func testPrices(context.Context) (price.Table, error) {
	return price.Parse([]byte(`{"data":[
		{"id":"openai/gpt-5","pricing":{"prompt":"0.00000125","completion":"0.00001","input_cache_read":"0.000000125"}},
		{"id":"x-ai/grok-4.7","pricing":{"prompt":"0.000002","completion":"0.000006"}}]}`))
}

func usedStep(text string, input, output int) fake.Step {
	s := fake.Reply(text)
	s.Usage = agent.Usage{Input: input, Output: output}
	return s
}

// A model of the SDK's list is priced with no setting: the turn line
// shows what it cost.
func TestListPriceShown(t *testing.T) {
	h := newHarness(t, "", usedStep("ok", 1_000_000, 100_000))
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "gpt-5", "OPENAI_API_KEY": "k"})
	if code := h.run("run", "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.err.String(), "1000000 in / 100000 out tokens, $2.25]") {
		t.Fatalf("err %q", h.err)
	}
}

// ringil.yaml's price wins over the list; a model with neither shows no
// cost.
func TestConfiguredPrice(t *testing.T) {
	h := newHarness(t, "", usedStep("ok", 2_000_000, 0))
	writeConfig(t, h.dir, "prices:\n  m:\n    input: 0.5\n    output: 1\n")
	if code := h.run("run", "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.err.String(), "out tokens, $1.00]") {
		t.Fatalf("err %q", h.err)
	}
	h = newHarness(t, "", usedStep("ok", 2_000_000, 0))
	if code := h.run("run", "x"); code != exitOK || strings.Contains(h.err.String(), "$") {
		t.Fatalf("an unpriced model shows a cost: exit %d %q", code, h.err)
	}
}

// --max-cost stops the prompt once it has cost that much: the call that
// crossed it is kept, the next one is not made.
func TestMaxCostStopsThePrompt(t *testing.T) {
	step := fake.Call("calculate", `{"a":2,"op":"*","b":21}`)
	step.Usage = agent.Usage{Input: 1_000_000}
	h := newHarness(t, "", step, step, fake.Reply("never"))
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "gpt-5", "OPENAI_API_KEY": "k"})
	if code := h.run("--max-cost", "$2", "run", "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.err.String(), "[max_cost, 2 model calls") || h.script.Remaining() != 1 {
		t.Fatalf("err %q, %d steps left", h.err, h.script.Remaining())
	}
}

func TestBadDollarFlag(t *testing.T) {
	for _, v := range []string{"-1", "abc", "Inf", "NaN"} {
		h := newHarness(t, "", fake.Reply("ok"))
		if code := h.run("--max-cost", v, "run", "x"); code == exitOK || !strings.Contains(h.err.String(), "US dollars") {
			t.Errorf("--max-cost %s: exit %d %q", v, code, h.err)
		}
	}
}

func TestCostSettingsChecked(t *testing.T) {
	for _, f := range []*config{
		{MaxCost: -1},
		{WorkflowCost: math.Inf(1)},
		{Prices: map[string]priceEntry{"m": {Input: -1}}},
		{Prices: map[string]priceEntry{"": {Input: 1}}},
	} {
		if err := f.Validate(); err == nil {
			t.Errorf("%+v accepted", f)
		}
	}
}

// The file's limits are used when no flag is given, and a cache price
// left out is the input price.
func TestCostSettingsFromTheFile(t *testing.T) {
	h := newHarness(t, "")
	env(h, map[string]string{})
	writeConfig(t, h.dir, "max_cost: 0.75\nworkflow_cost: 3\nprices:\n  m:\n    input: 2\n    output: 8\n")
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		mc, err := h.c.flagDollars(ctx, "max-cost")
		if err != nil || mc != 0.75 {
			t.Errorf("max-cost %v %v", mc, err)
		}
		b, err := h.c.workflowBudget(ctx)
		if err != nil || b.Cost != 3 {
			t.Errorf("workflow budget %+v %v", b, err)
		}
		if p, ok := h.c.priceOf(ctx.Context(), "m"); !ok || p.CacheRead != 2 || p.CacheWrite != 2 {
			t.Errorf("price %+v %v", p, ok)
		}
		return nil
	})
}

func TestUSD(t *testing.T) {
	for v, want := range map[float64]string{0.004: "$0.0040", 0.0000183: "$0.000018", 1e-15: "$0.0000000000", 0.01: "$0.01", 2.255: "$2.25", 12: "$12.00", 0: "$0.00"} {
		if got := usd(v); got != want {
			t.Errorf("usd(%v) = %s, want %s", v, got, want)
		}
	}
}

// A model of the list with a dotted name is found as its provider names
// it; a model on this machine costs nothing; OpenRouter is asked for its
// own cost.
func TestPricedEndpoints(t *testing.T) {
	h := newHarness(t, "", usedStep("ok", 1_000_000, 0))
	env(h, map[string]string{"RINGIL_PROVIDER": "xai", "RINGIL_MODEL": "grok-4.7", "XAI_API_KEY": "k"})
	if code := h.run("run", "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.err.String(), ", $2.00]") {
		t.Fatalf("grok: %q", h.err)
	}
	for base, free := range map[string]bool{"http://127.0.0.1:11434/v1": true, "http://localhost:8080/v1": true, "https://api.x.ai/v1": false} {
		if local(base) != free {
			t.Errorf("local(%s) = %v", base, !free)
		}
	}
	if !statesCost("https://openrouter.ai/api/v1") || statesCost("http://openrouter.ai/api/v1") || statesCost("https://router.requesty.ai/v1") {
		t.Error("only OpenRouter, over HTTPS, states its cost")
	}
}

// A dollar limit on a model with no price would never stop anything:
// refused, and said how to fix.
func TestDollarLimitNeedsAPrice(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	if code := h.run("--max-cost", "1", "run", "x"); code == exitOK || !strings.Contains(h.err.String(), "prices:") {
		t.Fatalf("exit %d %q", code, h.err)
	}
}

// When the list cannot be had, it is said once and the run goes on.
func TestPriceListDown(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	h.c.prices = func(context.Context) (price.Table, error) { return nil, errors.New("offline") }
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "gpt-5", "OPENAI_API_KEY": "k"})
	if code := h.run("run", "x"); code != exitOK || strings.Count(h.err.String(), "* prices:") != 1 {
		t.Fatalf("exit %d %q", code, h.err)
	}
}

func TestReadsPDF(t *testing.T) {
	for base, want := range map[string]bool{"https://api.openai.com/v1": true, "https://openrouter.ai/api/v1": true, "http://openrouter.ai/api/v1": false, "https://api.deepseek.com": false, "http://127.0.0.1:11434/v1": false} {
		if readsPDF(base) != want {
			t.Errorf("%s: %v", base, !want)
		}
	}
}
