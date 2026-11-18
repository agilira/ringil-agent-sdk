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

type scored struct {
	rec   agent.MemoryRecord
	score float64
}

// rank returns up to limit records for query, best first; ties and an
// empty query favour the most recent. Records sharing no word with the
// query are left out.
func rank(recs []agent.MemoryRecord, query string, limit int) []agent.MemoryRecord {
	if limit <= 0 {
		return nil
	}
	terms := unique(tokens(query))
	docs := make([][]string, len(recs))
	for i, r := range recs {
		docs[i] = tokens(r.Content)
	}
	var hits []scored
	for i, r := range recs {
		if s := bm25(terms, docs, i); s > 0 || len(terms) == 0 {
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

func bm25(terms []string, docs [][]string, i int) float64 {
	avg := averageLength(docs)
	score := 0.0
	for _, t := range terms {
		tf := float64(count(docs[i], t))
		if tf == 0 {
			continue
		}
		df := float64(documentsWith(docs, t))
		idf := math.Log(1 + (float64(len(docs))-df+0.5)/(df+0.5))
		norm := tf + k1*(1-b+b*float64(len(docs[i]))/avg)
		score += idf * tf * (k1 + 1) / norm
	}
	return score
}

func averageLength(docs [][]string) float64 {
	total := 0
	for _, d := range docs {
		total += len(d)
	}
	return math.Max(float64(total)/float64(max(len(docs), 1)), 1)
}

func count(doc []string, t string) int {
	n := 0
	for _, w := range doc {
		if w == t {
			n++
		}
	}
	return n
}

func documentsWith(docs [][]string, t string) int {
	n := 0
	for _, d := range docs {
		if count(d, t) > 0 {
			n++
		}
	}
	return n
}
