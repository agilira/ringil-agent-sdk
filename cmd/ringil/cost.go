// cost.go: what the model calls cost, and the dollar limits
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"maps"
	"math"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/price"
)

// priceEntry is a model's price in ringil.yaml, US dollars per million
// tokens. A cache price left out is the input price: no discount is
// assumed that the vendor does not give.
type priceEntry struct {
	Input      float64 `argus:"input"`
	Output     float64 `argus:"output"`
	CacheRead  float64 `argus:"cache_read"`
	CacheWrite float64 `argus:"cache_write"`
}

func (e priceEntry) price() price.Price {
	p := price.Price{Input: e.Input, Output: e.Output, CacheRead: e.CacheRead, CacheWrite: e.CacheWrite}
	if p.CacheRead == 0 {
		p.CacheRead = p.Input
	}
	if p.CacheWrite == 0 {
		p.CacheWrite = p.Input
	}
	return p
}

func (e priceEntry) check(name string) error {
	switch {
	case name == "" || !dollars(e.Input) || !dollars(e.Output) || !dollars(e.CacheRead) || !dollars(e.CacheWrite):
		return errors.New(ErrUsage, "prices."+name+": prices are US dollars per million tokens, not below zero")
	case e.Input > 0 && e.Output == 0:
		return errors.New(ErrUsage, "prices."+name+": output is missing (0 only for a free model)")
	}
	return nil
}

// dollars says whether v is a usable amount: a number, not below zero.
func dollars(v float64) bool { return v >= 0 && !math.IsInf(v, 0) }

func (f *config) checkCosts() error {
	if !dollars(f.MaxCost) || !dollars(f.WorkflowCost) {
		return errors.New(ErrUsage, "max_cost and workflow_cost are amounts in US dollars, not below zero")
	}
	for _, name := range slices.Sorted(maps.Keys(f.Prices)) {
		if err := f.Prices[name].check(name); err != nil {
			return err
		}
	}
	return nil
}

// priceList is the downloaded list of prices (price.Catalog), refreshed
// once a day; ringil.yaml's prices go over it.
func (c *cli) priceList(ctx context.Context) (price.Table, error) {
	dir, err := c.dataDir()
	if err != nil {
		return nil, err
	}
	return (&price.Catalog{Path: filepath.Join(dir, "prices.json")}).Load(ctx)
}

// priceOf is the price of model: ringil.yaml's, else the downloaded
// list's. A list that cannot be had is said once, and the model's own
// price in ringil.yaml still counts.
func (c *cli) priceOf(ctx context.Context, model string) (price.Price, bool) {
	mine := price.Table{}
	for name, e := range c.conf().Prices {
		mine[name] = e.price()
	}
	if p, ok := mine.Lookup(model); ok {
		return p, true
	}
	load := c.prices
	if load == nil {
		load = c.priceList
	}
	list, err := load(ctx)
	if err != nil && c.priceWarned.CompareAndSwap(false, true) {
		_ = c.line("  * prices: " + describe(err) + (map[bool]string{true: " (using the saved list)", false: ""})[len(list) > 0])
	}
	return list.Lookup(model)
}

// priced puts the cost on p's answers. A ChatGPT sign-in is paid by
// subscription, and a model on this machine costs nothing: both are
// priced at zero. OpenRouter states each call's cost, which wins; a model
// with no price is remembered, for the dollar limits.
func (c *cli) priced(ctx context.Context, p agent.Provider, ep endpoint, model string) agent.Provider {
	if ep.kind == providerChatGPT || local(ep.base) {
		return price.Wrap(p, price.Price{})
	}
	pr, ok := c.priceOf(ctx, model)
	if !ok {
		c.unpriced = model
		return p
	}
	return price.Wrap(p, pr)
}

// local says whether base is on this machine (Ollama, llama.cpp, vLLM).
func local(base string) bool {
	u, err := url.Parse(base)
	return err == nil && (u.Hostname() == "localhost" || loopback(u.Hostname()))
}

// readsPDF: the API takes PDFs as files in Chat Completions (OpenAI's,
// and OpenRouter's for every model). Others are told a PDF is there.
func readsPDF(base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" {
		return false
	}
	return u.Hostname() == "api.openai.com" || u.Hostname() == "openrouter.ai"
}

// statesCost: OpenRouter puts each call's cost in its answer.
func statesCost(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Scheme == "https" && u.Hostname() == "openrouter.ai"
}

// checkLimits refuses a dollar limit on a model with no price: it would
// never stop anything.
func (c *cli) checkLimits(name string, usd float64) error {
	if usd > 0 && c.unpriced != "" {
		return errors.New(ErrUsage, "--"+name+": "+c.unpriced+" has no price in the downloaded list; add it under prices: in ringil.yaml")
	}
	return nil
}

// runModel is the run's model, and what a prompt may cost with it.
func (c *cli) runModel(ctx *orpheus.Context) (agent.Provider, error) {
	maxCost, err := c.flagDollars(ctx, "max-cost")
	if err != nil {
		return nil, err
	}
	c.maxCost = maxCost
	p, err := c.provider(ctx)
	if err != nil {
		return nil, err
	}
	return p, c.checkLimits("max-cost", maxCost)
}

// flagDollars is a dollar setting: the flag, else ringil.yaml's value.
func (c *cli) flagDollars(ctx *orpheus.Context, name string) (float64, error) {
	if !ctx.GlobalFlagChanged(name) {
		return c.conf().floatOf(name), nil
	}
	s := strings.TrimPrefix(strings.TrimSpace(ctx.GetGlobalFlagString(name)), "$")
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || !dollars(v) {
		return 0, errors.New(ErrUsage, "--"+name+" is an amount in US dollars, such as 0.50")
	}
	return v, nil
}

func (f *config) floatOf(name string) float64 {
	return map[string]float64{"max-cost": f.MaxCost, "workflow-cost": f.WorkflowCost}[name]
}

// usd is an amount for the terminal: cents, and for what costs less
// than a cent its first two significant digits.
func usd(v float64) string {
	decimals := 2
	if v > 0 && v < 0.01 {
		decimals = min(1-int(math.Floor(math.Log10(v))), 10)
	}
	return "$" + strconv.FormatFloat(v, 'f', decimals, 64)
}
