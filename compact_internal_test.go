// compact_internal_test.go: internals of the compactor
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"strconv"
	"testing"
)

func TestSummaryCacheIsBounded(t *testing.T) {
	c := &compactor{}
	for i := range summaryCacheSize * 3 {
		c.store(cachedSummary{n: i + 1, hash: strconv.Itoa(i)})
	}
	if len(c.summaries) != summaryCacheSize || c.summaries[0].n != 2*summaryCacheSize+1 {
		t.Fatalf("cache %d entries, oldest n=%d", len(c.summaries), c.summaries[0].n)
	}
}

func TestRatioIsClamped(t *testing.T) {
	c := &compactor{}
	c.learn(1_000_000, 1)
	if c.tokens(1600) != 100 {
		t.Fatalf("ratio not capped: %d", c.tokens(1600))
	}
	c.learn(10, 1000)
	if c.tokens(100) != 100 {
		t.Fatalf("ratio not floored: %d", c.tokens(100))
	}
	c.learn(0, 5)
	c.learn(5, 0)
	if c.tokens(100) != 100 {
		t.Fatal("zero values changed the ratio")
	}
}

func TestClipAndFitTailKeepRunes(t *testing.T) {
	if got := clip("àààà", 3); got != "à [...]" {
		t.Fatalf("clip %q", got)
	}
	if got := fitTail("àààà", 3); got != "[earlier part omitted]\nà" {
		t.Fatalf("fitTail %q", got)
	}
	if got := fitTail("abc", 0); got != "[transcript omitted]" {
		t.Fatalf("fitTail zero %q", got)
	}
}
