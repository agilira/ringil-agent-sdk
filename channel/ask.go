// ask.go: the server's approvals, asked and answered in the chat
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	agent "github.com/agilira/ringil-agent-sdk"
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
// run an owner started, any call is allowed without asking when the host
// turned the questions off (Config.NoApprovals); in such a run or in a
// group, so is a call an owner allowed "always" in the conversation.
func (b *Bridge) ask(ctx context.Context, chatID, run string, p serve.Approval) {
	a := &ask{token: rand.Text()[:16], chat: chatID, run: run, approval: p.ID, key: alwaysKey(p)}
	refusal := b.tr(chatID, mOwnerApproves)
	b.mu.Lock()
	c := b.chats[chatID]
	a.conv = c.conv
	switch {
	case (c.role == owner && b.people().noApprovals) || ((c.role == owner || c.group) && b.always[a.conv+"\x00"+a.key]):
		b.mu.Unlock()
		_ = b.cfg.Server.Decide(run, p.ID, agent.Answer{Allow: true})
		return
	case c.role != owner && !c.group:
		_ = b.cfg.Server.Decide(run, p.ID, agent.Answer{}) // under b.mu: its note goes out before the run's reply
		b.queue(ctx, chatID, outgoing{out: Out{Text: refusal}})
		b.mu.Unlock()
		return
	}
	b.asks[a.token] = a
	c.open = append(c.open, a)
	b.mu.Unlock()
	b.post(ctx, chatID, b.question(a, p, c.group), func() {
		b.mu.Lock()
		b.close(a)
		b.mu.Unlock()
	})
}

// question is how an approval is asked in its chat: in words, a preview
// of what it is about (all of it on request), the buttons.
func (b *Bridge) question(a *ask, p serve.Approval, group bool) Out {
	chatID := a.chat
	q := describe(b.lang(chatID), p.Tool, p.Input)
	if p.Worker != "" {
		q.text = b.tr(chatID, mWorkerAsks, inline(visible(p.Worker))) + q.text
	}
	if group {
		q.text += b.tr(chatID, mInGroup)
	}
	buttons := []Button{{ID: a.token + ":y", Label: b.tr(chatID, mYes)}, {ID: a.token + ":a", Label: b.tr(chatID, mAlways)}, {ID: a.token + ":n", Label: b.tr(chatID, mNo)}}
	if askEveryTime[p.Tool] {
		buttons = []Button{buttons[0], buttons[2]}
	}
	code, lines, chars := preview(q.code)
	if lines > 0 || chars > 0 {
		b.mu.Lock()
		a.full = capped(q.code)
		b.mu.Unlock()
		if lines > 0 {
			q.text += "\n" + b.tr(chatID, mMoreLines, strconv.Itoa(lines))
		} else {
			q.text += "\n" + b.tr(chatID, mMoreChars, strconv.Itoa(chars))
		}
		buttons = append(buttons, Button{ID: a.token + ":s", Label: b.tr(chatID, mShowAll)})
	}
	return Out{Text: q.text, Code: code, Buttons: buttons}
}

// askEveryTime are the tools asked about every time, without "always":
// each call is something new to read (a skill's instructions).
var askEveryTime = map[string]bool{"create_skill": true, "create_trigger": true}

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

// preview is the start of code an approval shows, and what it leaves
// out: whole lines, or (when it cuts inside a line) characters; both 0
// when it shows all.
func preview(code string) (shown string, lines, chars int) {
	all := strings.Split(code, "\n")
	head := all[:min(len(all), previewLines)]
	out := strings.Join(head, "\n")
	if r := []rune(out); len(r) > previewChars {
		out = string(r[:previewChars])
		return out, 0, len([]rune(code)) - previewChars
	}
	return out, len(all) - len(head), 0
}

// alwaysKey is what "always" allows: the file asked about (write_file,
// edit_file, write_protected_file), the command (only that command), the
// domain; for another tool, the tool.
func alwaysKey(p serve.Approval) string {
	var in struct {
		Command string `json:"command"`
		Domain  string `json:"domain"`
		Path    string `json:"path"`
	}
	_ = json.Unmarshal(p.Input, &in)
	switch {
	case strings.HasPrefix(p.Tool, "bash_"):
		return p.Tool + "\x00" + in.Command
	case p.Tool == "network_domain":
		return p.Tool + "\x00" + in.Domain
	case p.Tool == "write_file" || p.Tool == "edit_file" || p.Tool == "write_protected_file":
		return p.Tool + "\x00" + filepath.Clean(in.Path)
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
	if choice == "a" && !askEveryTime[a.key] {
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
	reply := b.tr(a.chat, mRefused)
	if allow {
		reply = b.tr(a.chat, mAllowed)
	}
	closed := b.tr(a.chat, mClosed)
	// The answer is queued with the decision, under the outbox's lock: the
	// run it wakes posts after it, never before (found 21/11: "Refused."
	// came after the run's reply under load). Decide only signals the run.
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.cfg.Server.Decide(a.run, a.approval, agent.Answer{Allow: allow}); err != nil {
		reply = closed
	}
	b.queue(ctx, a.chat, outgoing{out: Out{Text: reply}})
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
