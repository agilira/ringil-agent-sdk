// ask.go: the server's approvals, asked and answered in the chat
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/agilira/ringil-agent-sdk/serve"
)

// ask is an approval asked in a chat.
type ask struct {
	token, chat, conv, run, approval, key string
	full                                  string // the code shown in part, all of it
}

// ask shows an approval in the chat, with yes, always and no buttons. It
// is open from before it is sent (a quick press finds it) and closed if
// the sending fails: nobody answers a question they did not see.
//
// Only owners approve: in the private chat of anyone else nobody could,
// so the call is refused at once; in a group, an owner there may. In a
// run an owner started, a call allowed "always" in the conversation, or
// any call when the host turned the questions off (Config.NoApprovals),
// is allowed without asking.
func (b *Bridge) ask(ctx context.Context, chatID, run string, p serve.Approval) {
	a := &ask{token: rand.Text()[:16], chat: chatID, run: run, approval: p.ID, key: alwaysKey(p)}
	b.mu.Lock()
	c := b.chats[chatID]
	a.conv = c.conv
	switch {
	case c.role == owner && (b.cfg.NoApprovals || b.always[a.conv+"\x00"+a.key]):
		b.mu.Unlock()
		_ = b.cfg.Server.Decide(run, p.ID, true)
		return
	case c.role != owner && !c.group:
		b.mu.Unlock()
		_ = b.cfg.Server.Decide(run, p.ID, false)
		b.say(ctx, chatID, b.tr(chatID, mOwnerApproves))
		return
	}
	b.asks[a.token] = a
	c.open = append(c.open, a)
	b.mu.Unlock()
	q := describe(b.lang(chatID), p.Tool, p.Input)
	if p.Worker != "" {
		q.text = b.tr(chatID, mWorkerAsks, visible(p.Worker)) + q.text
	}
	if c.group {
		q.text += b.tr(chatID, mInGroup)
	}
	buttons := []Button{{ID: a.token + ":y", Label: b.tr(chatID, mYes)}, {ID: a.token + ":a", Label: b.tr(chatID, mAlways)}, {ID: a.token + ":n", Label: b.tr(chatID, mNo)}}
	code, more := preview(q.code)
	if more > 0 {
		b.mu.Lock()
		a.full = q.code
		b.mu.Unlock()
		q.text += "\n" + b.tr(chatID, mMoreLines, strconv.Itoa(more))
		buttons = append(buttons, Button{ID: a.token + ":s", Label: b.tr(chatID, mShowAll)})
	}
	b.post(ctx, chatID, Out{Text: q.text, Code: code, Buttons: buttons}, func() {
		b.mu.Lock()
		b.close(a)
		b.mu.Unlock()
	})
}

// validChoice: yes, always, no, or show all.
func validChoice(c string) bool {
	return c == "y" || c == "a" || c == "n" || c == "s"
}

// An approval shows at most previewLines lines, previewChars characters,
// of what it is about; the rest on request.
const (
	previewLines = 15
	previewChars = 1500
)

// preview is the start of code an approval shows, and how many lines it
// leaves out (0: all of it shown).
func preview(code string) (string, int) {
	lines := strings.Split(code, "\n")
	shown := lines[:min(len(lines), previewLines)]
	out := strings.Join(shown, "\n")
	if r := []rune(out); len(r) > previewChars {
		out = string(r[:previewChars])
		shown = strings.Split(out, "\n")
	}
	if out == code {
		return code, 0
	}
	return out, max(len(lines)-len(shown), 1)
}

// alwaysKey is what "always" allows: a tool, a command (only that
// command), a domain.
func alwaysKey(p serve.Approval) string {
	var in struct {
		Command string `json:"command"`
		Domain  string `json:"domain"`
	}
	_ = json.Unmarshal(p.Input, &in)
	switch {
	case strings.HasPrefix(p.Tool, "bash_"):
		return p.Tool + "\x00" + in.Command
	case p.Tool == "network_domain":
		return p.Tool + "\x00" + in.Domain
	}
	return p.Tool
}

// press decides the approval a button names, if it is of this chat and
// an owner pressed it.
func (b *Bridge) press(ctx context.Context, m Message, r role) {
	if r != owner {
		b.say(ctx, m.Chat, b.tr(m.Chat, mOwnerAnswers))
		return
	}
	token, choice, _ := strings.Cut(m.Pressed, ":")
	b.mu.Lock()
	a := b.asks[token]
	if a == nil || a.chat != m.Chat || !validChoice(choice) {
		b.mu.Unlock()
		b.say(ctx, m.Chat, b.tr(m.Chat, mClosed))
		return
	}
	if choice == "s" { // the whole code; the question stays open
		full := a.full
		b.mu.Unlock()
		b.post(ctx, m.Chat, Out{Text: b.tr(m.Chat, mWhole), Code: full}, nil)
		return
	}
	b.close(a)
	if choice == "a" {
		if len(b.always) >= maxTracked {
			clear(b.always) // asked again: never allowed by mistake
		}
		b.always[a.conv+"\x00"+a.key] = true
	}
	b.mu.Unlock()
	b.decide(ctx, a, choice != "n")
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
		b.say(ctx, chatID, b.tr(chatID, mSeveral))
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
		b.say(ctx, a.chat, b.tr(a.chat, mClosed))
		return
	}
	if allow {
		b.say(ctx, a.chat, b.tr(a.chat, mAllowed))
	} else {
		b.say(ctx, a.chat, b.tr(a.chat, mRefused))
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
