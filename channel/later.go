// later.go: the checks a run sets come back to its chat
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/later"
)

// Later is the source (serve.SourceOf) of the runs ComeBack starts: a
// check the agent set with check_later, come due.
const Later = "later"

// Errors of ComeBack.
const (
	// ErrBusy: the chat is in a run; offer the check again shortly.
	ErrBusy errors.ErrorCode = "CHANNEL_BUSY"
	// ErrGone: the conversation is no longer the chat's (a new one
	// started), or who set the check may no longer talk: stop it.
	ErrGone errors.ErrorCode = "CHANNEL_GONE"
)

// back is where a conversation's checks come back: the private chat and
// the person in it.
type back struct{ chat, from string }

// maxBacks bounds the conversations remembered for ComeBack; the oldest
// is dropped first (its checks then end).
const maxBacks = 1000

// Private reports whether conversation is a private chat's, in a run now
// of an owner or a member, so that checks set in it can come back
// (ComeBack). Group chats have none (a check would run for whoever spoke
// last), nor do the public's chats.
func (b *Bridge) Private(conversation string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, c := range b.chats {
		if c.conv == conversation && !c.group && (c.role == owner || c.role == member) {
			if _, known := b.backs[conversation]; !known {
				if len(b.backs) >= maxBacks {
					b.dropOldestBack()
				}
				b.backOrder = append(b.backOrder, conversation)
			}
			b.backs[conversation] = back{chat: id, from: c.from}
			return true
		}
	}
	return false
}

// dropOldestBack forgets the conversation remembered first. b.mu held.
func (b *Bridge) dropOldestBack() {
	for len(b.backOrder) > 0 {
		oldest := b.backOrder[0]
		b.backOrder = b.backOrder[1:]
		if _, ok := b.backs[oldest]; ok {
			delete(b.backs, oldest)
			return
		}
	}
}

// ComeBack starts a run of a check come due (text, later.Notice's) in the
// chat of conversation, as the person who set it, with the usual
// approvals. Its answer goes to the chat unless it is later.Quiet. It
// fails with ErrBusy while the chat is in a run and with ErrGone when the
// check should end.
func (b *Bridge) ComeBack(conversation, text string) error {
	b.mu.Lock()
	ctx, at := b.running, b.backs[conversation]
	b.mu.Unlock()
	ok := at.chat != "" && b.current(at.chat) == conversation
	switch {
	case ctx == nil:
		return errors.New(ErrStopped, "the bridge is not running")
	case !ok:
		return errors.New(ErrGone, "the conversation is no longer the chat's")
	}
	r := b.roleOf(ctx, at.from)
	if r != owner && r != member {
		return errors.New(ErrGone, "who set the check may no longer talk here")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running != ctx { // Run ended meanwhile: it waits for no new run
		return errors.New(ErrStopped, "the bridge is not running")
	}
	if b.chats[at.chat] != nil {
		return errors.New(ErrBusy, "the chat is in a run")
	}
	prep, cancel := context.WithCancel(ctx)
	b.chats[at.chat] = &chat{conv: conversation, role: r, from: at.from, cancel: cancel, source: Later}
	b.wg.Add(1)
	go b.converse(ctx, prep, at.chat, text, nil)
	return nil
}

// quiet: a check that found nothing to tell; its chat hears nothing.
func quiet(source, text string) bool {
	return source == Later && later.IsQuiet(text)
}
