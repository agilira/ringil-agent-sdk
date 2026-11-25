// typing.go: 👀 on the message the agent is working on
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package slack

import (
	"context"
	"sync"
)

// reaction is what shows that the agent is on it: Slack has no typing
// for apps, so the message being answered gets 👀 until the answer.
const reaction = "eyes"

// marks are the messages being answered, by chat, and those with 👀.
type marks struct {
	mu     sync.Mutex
	last   map[string]string // chat: the last message received
	marked map[string]string // chat: the message that has 👀
}

func newMarks() *marks {
	return &marks{last: map[string]string{}, marked: map[string]string{}}
}

// heard records the last message of a chat; at most maxThreads chats.
func (m *marks) heard(chat, ts string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.last) >= maxThreads {
		clear(m.last)
	}
	m.last[chat] = ts
}

// toMark is the message to put 👀 on, once per message.
func (m *marks) toMark(chat string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ts, ok := m.last[chat]
	if !ok || m.marked[chat] == ts {
		return "", false
	}
	m.marked[chat] = ts
	return ts, true
}

// toUnmark is the message whose 👀 the answer takes away.
func (m *marks) toUnmark(chat string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ts, ok := m.marked[chat]
	delete(m.marked, chat)
	return ts, ok
}

type reactionIn struct {
	Channel   string `json:"channel"`
	Timestamp string `json:"timestamp"`
	Name      string `json:"name"`
}

// Typing puts 👀 on the message the agent is answering, once; the answer
// takes it away. It is cosmetic: Slack refusing it is not an error.
func (c *Channel) Typing(ctx context.Context, chat string) error {
	ts, ok := c.marks.toMark(chat)
	if !ok {
		return nil
	}
	ch, _ := splitChat(chat)
	_ = c.call(ctx, c.bot, "reactions.add", reactionIn{Channel: ch, Timestamp: ts, Name: reaction}, nil)
	return nil
}

// unmark takes 👀 away from the chat's message, if it has it.
func (c *Channel) unmark(ctx context.Context, chat string) {
	ts, ok := c.marks.toUnmark(chat)
	if !ok {
		return
	}
	ch, _ := splitChat(chat)
	_ = c.call(ctx, c.bot, "reactions.remove", reactionIn{Channel: ch, Timestamp: ts, Name: reaction}, nil)
}
