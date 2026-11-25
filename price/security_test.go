// security_test.go: the price package at its limits
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package price

import (
	"math"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
)

// FuzzLookup: any name, from a model's answer or a router, never panics.
func FuzzLookup(f *testing.F) {
	for _, s := range []string{"claude-opus-5-5", "a/b/gpt-5-20250101", "-2025-01-01", "é/ü-12345678", "\x00"} {
		f.Add(s)
	}
	tab := fixture(f)
	f.Fuzz(func(t *testing.T, name string) {
		if p, ok := tab.Lookup(name); ok && p.Input <= 0 {
			t.Fatalf("%q: empty price", name)
		}
	})
}

// FuzzParse: a list from the network, whatever it holds, never panics
// and never yields a negative or endless price.
func FuzzParse(f *testing.F) {
	f.Add([]byte(`{"data":[{"id":"a/b","pricing":{"prompt":"0.000001","completion":"0.000002","overrides":[{"min_prompt_tokens":10,"prompt":"0.000003"}]}}]}`))
	f.Add([]byte(`{"data":[{"id":"a/b","pricing":{"prompt":"1e-400","completion":"NaN"}}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		tab, err := Parse(b)
		if err != nil {
			return
		}
		for name, p := range tab {
			for q := &p; q != nil; q = q.Long {
				for _, v := range []float64{q.Input, q.Output, q.CacheRead, q.CacheWrite} {
					if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
						t.Fatalf("%s: %+v", name, q)
					}
				}
			}
		}
	})
}

// FuzzCost: whatever counts a provider reports, the cost is a finite
// number not below zero.
func FuzzCost(f *testing.F) {
	f.Add(1, 2, 3, 4)
	f.Add(math.MaxInt, math.MaxInt, math.MaxInt, math.MaxInt)
	f.Add(math.MinInt, 0, math.MaxInt, -1)
	p, _ := fixture(f).Lookup("claude-haiku-5-5")
	f.Fuzz(func(t *testing.T, input, output, read, write int) {
		c := p.Cost(agent.Usage{Input: input, Output: output, CacheRead: read, CacheWrite: write})
		if c < 0 || math.IsNaN(c) || math.IsInf(c, 0) {
			t.Fatalf("cost %v", c)
		}
	})
}
