// later.go: check_later in the terminal and in the chats
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	stderrors "errors"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/later"
)

// terminalChecks is the conversation key of the terminal's checks: one
// conversation per process.
const terminalChecks = "terminal"

// laterTools are check_later and stop_checking where a check can come
// back: the terminal's chat and run, and serve's private chats. Not in a
// trigger's runs (they end) nor in the API's (nobody to come back to).
func (c *cli) laterTools() []agent.Tool {
	switch {
	case c.board == nil || c.inTrigger:
		return nil
	case c.server != nil && !c.chatRun():
		return nil
	}
	return later.Tools(c.board, c.checkConversation)
}

// checkConversation is the key of the conversation a check comes back
// to: the terminal's, or the chat's when it is private.
func (c *cli) checkConversation(context.Context) string {
	if c.server == nil {
		return terminalChecks
	}
	if b := c.bridge.Load(); b != nil && b.Private(c.runConversation) {
		return c.runConversation
	}
	return ""
}

// terminalBoard sets up the checks of `ringil chat` and `ringil run`. A
// check come due is handed over only while the conversation waits for
// one (the prompt, or a run waiting for its checks): during a turn it is
// offered again later, uncounted, and nothing is left queued to run
// after a /reset or a stop.
func (c *cli) terminalBoard() {
	c.due = make(chan later.Check)
	c.board = later.NewBoard(c.laterLimits, func(ch later.Check) bool {
		select {
		case c.due <- ch:
			return true
		default:
			return false
		}
	})
	c.closers = append(c.closers, func() error { c.board.Close(); return nil })
}

// serveBoard sets up serve's checks: one come due starts a run in its
// chat; a busy chat is offered it again, a chat gone ends it.
func (c *cli) serveBoard() {
	c.board = later.NewBoard(c.laterLimits, c.comeBack)
	c.closers = append(c.closers, func() error { c.board.Close(); return nil })
}

func (c *cli) comeBack(ch later.Check) bool {
	b := c.bridge.Load()
	if b == nil {
		return false
	}
	err := b.ComeBack(ch.Conversation, later.Notice(ch).Text())
	var e *errors.Error
	if err == nil || (stderrors.As(err, &e) && e.Code == channel.ErrBusy) {
		return err == nil
	}
	_ = c.board.Stop(ch.Conversation, ch.ID)
	return true
}

// waitChecks has `ringil run` stay for the checks it set, until the last
// (Ctrl-C stops them).
func (c *cli) waitChecks(ctx context.Context, a agent.Agent, conv agent.Conversation) error {
	for c.board != nil && c.board.Pending(terminalChecks) > 0 {
		c.note("waiting for " + strconv.Itoa(c.board.Pending(terminalChecks)) + " check(s); Ctrl-C stops")
		select {
		case ch := <-c.due:
			if err := c.checkTurn(ctx, a, conv, ch); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// checkTurn runs a check come due. Its words are held until the turn
// ends or calls a tool: a check with nothing new shows only a note.
func (c *cli) checkTurn(ctx context.Context, a agent.Agent, conv agent.Conversation, ch later.Check) error {
	c.note("check " + strconv.Itoa(ch.Done) + " of " + strconv.Itoa(ch.Max) + " (id " + strconv.Itoa(ch.ID) + ")")
	ctx, cancel := c.turnContext(context.WithValue(ctx, checkKey{}, true))
	defer cancel()
	var said strings.Builder
	defer func() { _ = c.flushSaid(&said, false) }() // what a check cut short said
	for ev, err := range a.Stream(ctx, conv, later.Notice(ch)) {
		if err != nil {
			return err
		}
		if ev.Kind == agent.EventText {
			said.WriteString(ev.Text)
			continue
		}
		if ev.Kind == agent.EventToolStart || ev.Kind == agent.EventDone {
			if err := c.flushSaid(&said, ev.Kind == agent.EventDone); err != nil {
				return err
			}
		}
		if err := c.render(ev); err != nil {
			return err
		}
	}
	return nil
}

// flushSaid prints what a check said, or a note when, at its end, it
// said only later.Quiet.
func (c *cli) flushSaid(said *strings.Builder, end bool) error {
	text := said.String()
	said.Reset()
	if text == "" {
		return nil
	}
	if end && later.IsQuiet(text) {
		c.note("nothing new")
		return nil
	}
	return c.render(agent.Event{Kind: agent.EventText, Text: text})
}

type checkKey struct{}

// checking: ctx is a check's turn in the terminal. Its calls that would
// ask are refused: the user may be typing, and their line would answer.
func checking(ctx context.Context) bool {
	v, _ := ctx.Value(checkKey{}).(bool)
	return v
}
