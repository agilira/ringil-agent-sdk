// rank.go: BM25 ranking of memories
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package local

import (
	"math"
	"sort"
	"strings"
	"unicode"

	agent "github.com/agilira/ringil-agent-sdk"
)

// BM25 parameters: the usual values.
const (
	k1 = 1.2
	b  = 0.75
)

// tokens splits text into lower-cased words of letters and digits, with
// accents kept: "perché" and "perche" differ, as they do to a reader.
func tokens(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// index holds what BM25 needs, computed in one pass over the records: the
// cost of a search is linear in the size of the memory.
type index struct {
	freqs []map[string]int // term counts per record
	lens  []int
	df    map[string]int // records containing each term
	avg   float64
}

func newIndex(recs []agent.MemoryRecord) *index {
	ix := &index{freqs: make([]map[string]int, len(recs)), lens: make([]int, len(recs)), df: map[string]int{}}
	total := 0
	for i, r := range recs {
		words := tokens(r.Content)
		f := make(map[string]int, len(words))
		for _, w := range words {
			f[w]++
		}
		for w := range f {
			ix.df[w]++
		}
		ix.freqs[i], ix.lens[i] = f, len(words)
		total += len(words)
	}
	ix.avg = math.Max(float64(total)/float64(max(len(recs), 1)), 1)
	return ix
}

func (ix *index) score(i int, terms []string) float64 {
	n := float64(len(ix.freqs))
	s := 0.0
	for _, t := range terms {
		tf := float64(ix.freqs[i][t])
		if tf == 0 {
			continue
		}
		df := float64(ix.df[t])
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		s += idf * tf * (k1 + 1) / (tf + k1*(1-b+b*float64(ix.lens[i])/ix.avg))
	}
	return s
}

type scored struct {
	rec   agent.MemoryRecord
	score float64
}

// Rank returns up to limit of recs for query by BM25, best first; ties
// and an empty query favour the most recent. Records sharing no word with
// the query are left out. Other memories rank with it the records they
// found by words, so that every store ranks words the same way.
func Rank(recs []agent.MemoryRecord, query string, limit int) []agent.MemoryRecord {
	if limit <= 0 {
		return nil
	}
	terms := unique(tokens(query))
	ix := newIndex(recs)
	var hits []scored
	for i, r := range recs {
		if s := ix.score(i, terms); s > 0 || len(terms) == 0 {
			hits = append(hits, scored{r, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].rec.Created.After(hits[j].rec.Created)
	})
	out := make([]agent.MemoryRecord, 0, min(limit, len(hits)))
	for _, h := range hits[:min(limit, len(hits))] {
		out = append(out, h.rec)
	}
	return out
}

// Terms are the distinct words of text as Rank sees them: lower-cased
// letters and digits.
func Terms(text string) []string { return unique(tokens(text)) }

// rrfK is the usual constant of reciprocal rank fusion.
const rrfK = 60

// Fuse merges two rankings of the same records by reciprocal rank: a
// record high in either, or present in both, comes first.
func Fuse(a, b []agent.MemoryRecord, limit int) []agent.MemoryRecord {
	score := map[string]float64{}
	recs := map[string]agent.MemoryRecord{}
	var order []string
	for _, list := range [][]agent.MemoryRecord{a, b} {
		for rank, r := range list {
			if _, seen := recs[r.ID]; !seen {
				recs[r.ID] = r
				order = append(order, r.ID)
			}
			score[r.ID] += 1 / float64(rrfK+rank+1)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return score[order[i]] > score[order[j]] })
	out := make([]agent.MemoryRecord, 0, min(limit, len(order)))
	for _, id := range order[:min(limit, len(order))] {
		out = append(out, recs[id])
	}
	return out
}

func unique(words []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range words {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}
