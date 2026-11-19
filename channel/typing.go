// typing.go: "typing…" in the chat while the agent works
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"sync/atomic"
	"time"
)

// typingEvery is how often the Bridge says it is typing: chat apps show
// it for about five seconds.
var typingEvery = 4 * time.Second

// typer shows a run's chat that the agent is working, until stopped.
type typer struct {
	waiting atomic.Bool // the run waits for the user: nothing is shown
	cancel  context.CancelFunc
	done    chan struct{}
}

func (t *typer) stop() {
	t.cancel()
	<-t.done
}

// typing starts showing it, when the channel can.
func (b *Bridge) typing(ctx context.Context, chatID string) *typer {
	ctx, cancel := context.WithCancel(ctx)
	t := &typer{cancel: cancel, done: make(chan struct{})}
	ty, ok := b.cfg.Channel.(Typer)
	if !ok {
		close(t.done)
		return t
	}
	go func() {
		defer close(t.done)
		tick := time.NewTicker(typingEvery)
		defer tick.Stop()
		for first := true; ; first = false {
			// The first right away: the user has just written.
			if first || !t.waiting.Load() {
				// Only cosmetic: a failure is not reported, the reply will be.
				_ = ty.Typing(ctx, chatID)
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return t
}
