// embedgate.go: calling an embedder that may be down
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package embedgate calls an agent.Embedder for the memories that search
// by meaning: texts clipped to what embedding models accept, a query
// embedded as one where the model is asymmetric, and, after a failure, a
// pause, so that a broken or misnamed model costs one call a minute and
// not one per turn.
package embedgate

import (
	"context"
	"sync"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
)

// MaxChars keeps a text within what embedding models accept (about 8k
// tokens); the start of a long memory carries its meaning.
const MaxChars = 8000

// Pause is how long the embedder is left alone after a failure.
const Pause = time.Minute

// Gate calls one embedder.
type Gate struct {
	emb     agent.Embedder
	mu      sync.Mutex
	resting time.Time
	now     func() time.Time
}

// New returns a gate for emb.
func New(emb agent.Embedder) *Gate { return &Gate{emb: emb, now: time.Now} }

// Embed returns one vector per text, or false while resting or when the
// call fails (which starts a rest, unless ctx ended). query embeds through
// EmbedQuery when the embedder has it.
func (g *Gate) Embed(ctx context.Context, texts []string, query bool) ([][]float32, bool) {
	g.mu.Lock()
	resting := g.now().Before(g.resting)
	g.mu.Unlock()
	if resting {
		return nil, false
	}
	clipped := make([]string, len(texts))
	for i, t := range texts {
		clipped[i] = Clip(t, MaxChars)
	}
	embed := g.emb.Embed
	if qe, ok := g.emb.(agent.QueryEmbedder); ok && query {
		embed = qe.EmbedQuery
	}
	vecs, err := embed(ctx, clipped)
	if err == nil && len(vecs) != len(texts) {
		err = context.DeadlineExceeded // a wrong answer counts as a failure, below
	}
	if err != nil {
		if ctx.Err() == nil {
			g.mu.Lock()
			g.resting = g.now().Add(Pause)
			g.mu.Unlock()
		}
		return nil, false
	}
	return vecs, true
}

// Clip cuts s to at most n bytes, on a character boundary.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// Wake ends a rest now: the next call reaches the embedder.
func (g *Gate) Wake() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resting = time.Time{}
}
