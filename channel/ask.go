// ask.go: the server's approvals, asked and answered in the chat
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"crypto/rand"
	"strings"

	"github.com/agilira/ringil-agent-sdk/serve"
)

// ask is an approval asked in a chat.
type ask struct {
	token, chat, run, approval, tool string
}

// ask shows an approval in the chat, with yes and no buttons. It is open
// from before it is sent (a quick press finds it) and closed if the
// sending fails: nobody answers a question they did not see.
func (b *Bridge) ask(ctx context.Context, chatID, run string, p serve.Approval) {
	a := &ask{token: rand.Text()[:16], chat: chatID, run: run, approval: p.ID, tool: visible(p.Tool)}
	b.mu.Lock()
	b.asks[a.token] = a
	b.chats[chatID].open = append(b.chats[chatID].open, a)
	b.mu.Unlock()
	who := "The agent"
	if p.Worker != "" {
		who = "Worker " + visible(p.Worker)
	}
	text := who + " wants to use " + a.tool + ":\n" + showInput(p.Input) + "\nAllow it?"
	buttons := []Button{{ID: a.token + ":y", Label: "Yes"}, {ID: a.token + ":n", Label: "No"}}
	b.post(ctx, chatID, Out{Text: text, Buttons: buttons}, func() {
		b.mu.Lock()
		b.close(a)
		b.mu.Unlock()
	})
}

// press decides the approval a button names, if it is of this chat.
func (b *Bridge) press(ctx context.Context, m Message) {
	token, choice, _ := strings.Cut(m.Pressed, ":")
	b.mu.Lock()
	a := b.asks[token]
	if a == nil || a.chat != m.Chat || (choice != "y" && choice != "n") {
		b.mu.Unlock()
		b.say(ctx, m.Chat, "That question is closed.")
		return
	}
	b.close(a)
	b.mu.Unlock()
	b.decide(ctx, a, choice == "y")
}

// answered decides the chat's open approval when text is a yes or a no
// and only one is open; with several, it asks for the buttons, which say
// which. False when none is open or text is something else.
func (b *Bridge) answered(ctx context.Context, chatID, text string) bool {
	yes, ok := answer(text)
	if !ok {
		return false
	}
	b.mu.Lock()
	c := b.chats[chatID]
	if c == nil || len(c.open) == 0 {
		b.mu.Unlock()
		return false
	}
	if len(c.open) > 1 {
		b.mu.Unlock()
		b.say(ctx, chatID, "Several questions are open: answer each with its buttons.")
		return true
	}
	a := c.open[0]
	b.close(a)
	b.mu.Unlock()
	b.decide(ctx, a, yes)
	return true
}

func (b *Bridge) decide(ctx context.Context, a *ask, allow bool) {
	b.stamp(a.chat)
	if err := b.cfg.Server.Decide(a.run, a.approval, allow); err != nil {
		b.say(ctx, a.chat, "That question is closed.")
		return
	}
	if allow {
		b.say(ctx, a.chat, "Allowed: "+a.tool+".")
	} else {
		b.say(ctx, a.chat, "Refused: "+a.tool+".")
	}
}

// close forgets an asked approval; b.mu held.
func (b *Bridge) close(a *ask) {
	delete(b.asks, a.token)
	if c := b.chats[a.chat]; c != nil {
		c.open = without(c.open, a)
	}
}

func without(list []*ask, a *ask) []*ask {
	out := list[:0:0]
	for _, x := range list {
		if x != a {
			out = append(out, x)
		}
	}
	return out
}

// prune closes the chat's approvals no longer waiting (decided by a
// timeout, or elsewhere).
func (b *Bridge) prune(chatID string, pending []serve.Approval) {
	waiting := map[string]bool{}
	for _, p := range pending {
		waiting[p.ID] = true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, a := range b.chats[chatID].open {
		if !waiting[a.approval] {
			b.close(a)
		}
	}
}
