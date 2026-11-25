// price.go: what a model call costs, from the model's price
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package price turns the tokens of a model call into US dollars.
//
// [Wrap] puts the cost on every response of a provider, in Usage.Cost, so
// it reaches the events, the result of a run, the workers' reports, the
// workflows' journal and the metrics with no other change. A budget that
// caps dollars wraps the priced provider:
//
//	p := budget.Wrap(price.Wrap(provider, pr))
//
// Prices are not written in the code: a [Catalog] downloads the public
// list of OpenRouter (every vendor it serves: Anthropic, OpenAI, Google,
// xAI, Mistral, Moonshot, Z.ai, DeepSeek, Qwen and more) and keeps it on
// disk; the host adds or replaces models (a negotiated discount, a model
// the list lacks) with its own [Table].
package price

import (
	"context"
	"math"
	"strings"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Price is what a model charges, in US dollars per million tokens.
type Price struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	// CacheRead is the price of prompt tokens read from the cache.
	CacheRead float64 `json:"cache_read"`
	// CacheWrite is the price of prompt tokens written to the cache; a
	// vendor that does not charge for writing has it equal to Input.
	CacheWrite float64 `json:"cache_write"`
	// LongFrom, when set, is the prompt length from which the whole call
	// pays Long: some models charge more for long prompts. Long may have a
	// longer tier of its own.
	LongFrom int    `json:"long_from,omitempty"`
	Long     *Price `json:"long,omitempty"`
}

// Cost returns what u cost at this price. The prompt is u.Input, cache
// included: the part read from the cache and the part written to it pay
// their own prices, the rest the input price.
func (p Price) Cost(u agent.Usage) float64 {
	cur, prev := &p, 0
	for cur.Long != nil && cur.LongFrom > prev && u.Input >= cur.LongFrom {
		prev = cur.LongFrom
		cur = cur.Long
	}
	if !cur.ratesOK() {
		return 0
	}
	read, write := max(u.CacheRead, 0), max(u.CacheWrite, 0)
	plain := max(u.Input-read-write, 0)
	c := (float64(plain)*cur.Input + float64(read)*cur.CacheRead +
		float64(write)*cur.CacheWrite + float64(max(u.Output, 0))*cur.Output) / 1e6
	if !(c >= 0) || math.IsInf(c, 0) {
		return 0
	}
	return c
}

func validRate(v float64) bool { return v >= 0 && !math.IsInf(v, 0) }

func (p Price) ratesOK() bool {
	return validRate(p.Input) && validRate(p.Output) && validRate(p.CacheRead) && validRate(p.CacheWrite)
}

func validPrice(p Price) bool {
	prev := 0
	for cur := &p; cur != nil; cur = cur.Long {
		if !cur.ratesOK() || (cur.Long != nil && cur.LongFrom <= prev) {
			return false
		}
		prev = cur.LongFrom
	}
	return true
}

// Table maps model names to their prices.
type Table map[string]Price

// Lookup finds the price of a model by the name its provider uses: as it
// is, then in lower case with dots as dashes ("grok-4.7" is "grok-4-7"),
// without a router's vendor prefix ("anthropic/claude-opus-5-5"), and
// last without a date suffix ("claude-haiku-4-5-20251001"). A name it
// does not know returns false.
func (t Table) Lookup(model string) (Price, bool) {
	for _, name := range candidates(model) {
		if p, ok := t[name]; ok {
			return p, true
		}
	}
	return Price{}, false
}

func candidates(model string) []string {
	out := []string{model, normal(model)}
	bare := model
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		bare = model[i+1:]
		out = append(out, bare, normal(bare))
	}
	if base, ok := strings.CutSuffix(bare, dateSuffix(bare)); ok && base != bare && base != "" {
		out = append(out, base, normal(base))
	}
	return out
}

// normal is the form a catalog stores names in.
func normal(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), ".", "-")
}

// dateSuffix returns the "-YYYYMMDD" or "-YYYY-MM-DD" at the end of name,
// or "".
func dateSuffix(name string) string {
	for _, shape := range []string{"-dddddddd", "-dddd-dd-dd"} {
		if len(name) <= len(shape) {
			continue
		}
		if tail := name[len(name)-len(shape):]; matches(tail, shape) {
			return tail
		}
	}
	return ""
}

func matches(s, shape string) bool {
	for i := range len(shape) {
		if shape[i] == 'd' && (s[i] < '0' || s[i] > '9') || shape[i] == '-' && s[i] != '-' {
			return false
		}
	}
	return true
}

// Over returns a table with t's prices and over's in their place: the
// host's own prices over a catalog. Keys are normalized to match Lookup.
func (t Table) Over(over Table) Table {
	out := make(Table, len(t)+len(over))
	for name, p := range t {
		out[normal(name)] = p
	}
	for name, p := range over {
		out[normal(name)] = p
	}
	return out
}

// Wrap returns p with the cost of every answered call in Usage.Cost. A
// cost the provider stated already is kept; a negative or infinite one is replaced.
func Wrap(p agent.Provider, price Price) agent.Provider {
	return &provider{next: p, price: price}
}

type provider struct {
	next  agent.Provider
	price Price
}

func (p *provider) Info() agent.ModelInfo { return p.next.Info() }

func (p *provider) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	resp, err := p.next.Generate(ctx, req, sink)
	if resp != nil && (!(resp.Usage.Cost > 0) || math.IsInf(resp.Usage.Cost, 0)) {
		resp.Usage.Cost = p.price.Cost(resp.Usage)
	}
	return resp, err
}
