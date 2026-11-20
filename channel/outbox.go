// outbox.go: replies sent in order, apart from receiving
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"

	"github.com/agilira/go-errors"
)

// maxQueued bounds the replies waiting for one chat; past it the oldest
// is dropped, and reported.
const maxQueued = 64

// outbox is a chat's replies waiting, sent one at a time in order.
type outbox struct {
	queue   []outgoing
	sending bool
}

type outgoing struct {
	out Out
	// failed, when set, runs if out could not be sent.
	failed func()
	// askPhone sends out with the PhoneAsker's button.
	askPhone bool
}

func (b *Bridge) say(ctx context.Context, chatID, text string) {
	b.post(ctx, chatID, Out{Text: text}, nil)
}

// post queues a reply for a chat and returns at once.
func (b *Bridge) post(ctx context.Context, chatID string, out Out, failed func()) {
	b.mu.Lock()
	b.queue(ctx, chatID, outgoing{out: out, failed: failed})
	b.mu.Unlock()
}

// queue adds a reply to a chat's outbox; b.mu held. A reply dropped for
// room is reported from its own goroutine: report must not run under
// b.mu.
func (b *Bridge) queue(ctx context.Context, chatID string, o outgoing) {
	box := b.boxes[chatID]
	if box == nil {
		box = &outbox{}
		b.boxes[chatID] = box
	}
	var dropped *outgoing
	if len(box.queue) >= maxQueued {
		d := box.queue[0]
		dropped = &d
		box.queue = box.queue[1:]
	}
	box.queue = append(box.queue, o)
	if dropped != nil {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.failed(chatID, *dropped, errors.New(ErrQueue, "too many replies waiting for this chat"))
		}()
	}
	if !box.sending {
		box.sending = true
		b.wg.Add(1)
		go b.drain(ctx, chatID, box)
	}
}

// drain sends a chat's replies until none is left.
func (b *Bridge) drain(ctx context.Context, chatID string, box *outbox) {
	defer b.wg.Done()
	for {
		b.mu.Lock()
		if len(box.queue) == 0 {
			box.sending = false
			delete(b.boxes, chatID)
			b.mu.Unlock()
			return
		}
		next := box.queue[0]
		box.queue = box.queue[1:]
		b.mu.Unlock()
		if err := b.deliver(ctx, chatID, next); err != nil {
			b.failed(chatID, next, err)
		}
	}
}

func (b *Bridge) failed(chatID string, o outgoing, err error) {
	if o.failed != nil {
		o.failed() // first: whoever hears of the failure finds it settled
	}
	b.report(Event{Kind: SendFailed, Chat: chatID, Err: err})
}

// deliver sends out: as voice when it has some and the channel can,
// else (or when the voice fails) as text. A voice that failed only in
// the app's answer (a timeout after it took it) is then also text: an
// answer twice rather than none.
func (b *Bridge) deliver(ctx context.Context, chatID string, o outgoing) error {
	out := o.out
	if pa, ok := b.cfg.Channel.(PhoneAsker); ok && o.askPhone {
		return pa.AskPhone(ctx, chatID, out.Text, b.tr(chatID, mPhoneButton))
	}
	voice := out.Voice
	out.Voice = nil
	if vs, ok := b.cfg.Channel.(VoiceSender); ok && voice != nil {
		err := vs.SendVoice(ctx, chatID, *voice)
		if err == nil {
			return nil
		}
		b.report(Event{Kind: VoiceFailed, Chat: chatID, Err: err})
	}
	return b.cfg.Channel.Send(ctx, chatID, out)
}
