// thinking.go: the thinking of a round of tool calls, kept for its turn
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package anthropic

import (
	"sync"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

// maxRounds bounds the rounds remembered: as many as agents share the
// provider at once, each in a round, with room to spare.
const maxRounds = 1024

// rounds keeps each turn that thought and called tools, whole, by its first
// call's ID: the API wants it back with the results, as it was made.
// Thinking is opaque (its text may be empty, its signature binds it to
// this conversation), so it never enters the agent's messages: a session
// saved and resumed goes back without it, which the API takes.
type rounds struct {
	mu    sync.Mutex
	byID  map[string][]anthropic.BetaContentBlockParamUnion
	order []string
}

func newRounds() *rounds {
	return &rounds{byID: map[string][]anthropic.BetaContentBlockParamUnion{}}
}

// keep remembers the turn whose first call is id.
func (r *rounds) keep(id string, turn []anthropic.BetaContentBlockParamUnion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; !ok {
		r.order = append(r.order, id)
	}
	r.byID[id] = turn
	for len(r.order) > maxRounds {
		delete(r.byID, r.order[0])
		r.order = r.order[1:]
	}
}

// turn is the turn whose first call is id, if kept.
func (r *rounds) turn(id string) []anthropic.BetaContentBlockParamUnion {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}
