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
// call's ID: the API wants it back as it was made, thinking interleaved
// with the calls where it was, which the message's parts (reasoning, then
// text and calls) do not keep. A session resumed elsewhere goes back from
// the parts: in the common order, thinking first, that is the same.
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
