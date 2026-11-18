// chat.go: the interactive chat and the one-shot run
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/todo"
)

// maxLine bounds one line typed at the prompt.
const maxLine = 1 << 20

func (c *cli) cmdRun(ctx *orpheus.Context) error {
	prompt := strings.TrimSpace(strings.Join(ctx.Positional(), " "))
	if prompt == "" {
		return errors.New(ErrUsage, `nothing to send; usage: ringil run "<prompt>"`)
	}
	defer c.shutdown()
	a, err := c.agent(ctx)
	if err != nil {
		return err
	}
	conv, release, err := c.conversation(ctx)
	if err != nil {
		return err
	}
	err = c.turn(ctx.Context(), a, conv, prompt)
	if err == nil {
		err = c.reports(ctx.Context(), a, conv, true)
	}
	return closing(err, release)
}

// closing returns err, or the release error when err is nil.
func closing(err error, release func() error) error {
	if rerr := release(); err == nil {
		return rerr
	}
	return err
}

func (c *cli) cmdChat(ctx *orpheus.Context) error {
	defer c.shutdown()
	a, err := c.agent(ctx)
	if err != nil {
		return err
	}
	conv, release, err := c.conversation(ctx)
	if err != nil {
		return err
	}
	return closing(c.chat(ctx.Context(), a, conv), release)
}

// promptLine reads the user's next message while holding the terminal, so
// that a background worker's question waits for the prompt instead of
// taking the user's message as its answer.
func (c *cli) promptLine() (string, bool) {
	c.askMu.Lock()
	defer c.askMu.Unlock()
	return c.readLine()
}

// exchange is one message of the chat: background reports that came in
// meanwhile first, then the user's turn, then reports already finished.
func (c *cli) exchange(ctx context.Context, a agent.Agent, conv agent.Conversation, line string) {
	steps := []func() error{
		func() error { return c.reports(ctx, a, conv, false) },
		func() error { return c.turn(ctx, a, conv, line) },
		func() error { return c.reports(ctx, a, conv, false) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			c.note("error: " + describe(err))
		}
	}
}

func (c *cli) chat(ctx context.Context, a agent.Agent, conv agent.Conversation) error {
	for c.prompt() {
		raw, ok := c.promptLine()
		if !ok {
			break
		}
		line := strings.TrimSpace(raw)
		switch line {
		case "":
		case "/exit", "/quit":
			return nil
		case "/reset":
			c.note("history cleared for this chat; a saved session is left as it was and no longer updated")
			conv = agent.NewConversation()
		default:
			c.exchange(ctx, a, conv, line)
		}
	}
	if c.lines != nil {
		return c.lines.Err()
	}
	return nil
}

func (c *cli) prompt() bool {
	_, err := fmt.Fprint(c.out, "\n> ")
	return err == nil
}

// turn sends one prompt and renders its events. Everything the run produced
// is already in conv, whether or not it finished.
func (c *cli) turn(ctx context.Context, a agent.Agent, conv agent.Conversation, prompt string) error {
	return c.turnWith(ctx, a, conv, agent.UserText(prompt))
}

func (c *cli) turnWith(ctx context.Context, a agent.Agent, conv agent.Conversation, msg agent.Message) error {
	ctx, cancel := c.turnContext(ctx)
	defer cancel()
	for ev, err := range a.Stream(ctx, conv, msg) {
		if err != nil {
			return err
		}
		if err := c.render(ev); err != nil {
			return err
		}
	}
	return nil
}

func (c *cli) render(ev agent.Event) error {
	var err error
	switch ev.Kind {
	case agent.EventText:
		_, err = fmt.Fprint(c.out, clean(ev.Text))
	case agent.EventToolStart:
		err = c.line(fmt.Sprintf("\n  -> %s %s", ev.Call.Name, short(string(ev.Call.Input))))
	case agent.EventToolEnd:
		err = c.toolEnd(ev)
	case agent.EventRecalled:
		err = c.recalled(ev)
	case agent.EventDone:
		err = c.done(ev)
	default:
		if s, ok := status(ev); ok {
			err = c.line(s)
		}
	}
	return err
}

// status is the one-line notice of an event that only informs.
func status(ev agent.Event) (string, bool) {
	switch ev.Kind {
	case agent.EventContext:
		return "  ! notes index unavailable: " + describe(ev.Err), true
	case agent.EventCompacted:
		return "  ~ context compacted: " + ev.Text, true
	case agent.EventLoop:
		return "  ! loop: " + ev.Call.Name + " keeps giving the same result (" + ev.Text + ")", true
	case agent.EventRecovered:
		return "  ! " + ev.Call.Name + " was interrupted earlier; the model was told, it was not run again", true
	}
	return "", false
}

func (c *cli) line(s string) error {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	_, err := fmt.Fprintln(c.errOut, s)
	return err
}

// note prints a status line; a failed write leaves nothing better to do.
func (c *cli) note(s string) {
	if err := c.line("  " + s); err != nil {
		return
	}
}

// short trims a tool input or result for display.
func short(s string) string {
	s = clean(strings.Join(strings.Fields(s), " "))
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "..."
	}
	return s
}

// clean removes control characters except newlines and tabs, and the
// bidirectional overrides that reorder text on screen. Model output and
// tool results are untrusted: an escape sequence printed raw could rewrite
// the terminal, fake a prompt or hide text.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || printable(r) {
			return r
		}
		return -1
	}, s)
}

func printable(r rune) bool {
	return !(r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)) && !hidden[r] && !(r >= 0x202a && r <= 0x202e) && !(r >= 0x2066 && r <= 0x2069)
}

// hidden are characters that reorder or hide text on screen: bidi marks,
// zero-width characters, line separators.
var hidden = map[rune]bool{
	0x200b: true, 0x200c: true, 0x200d: true, 0x200e: true, 0x200f: true,
	0x061c: true, 0x2060: true, 0xfeff: true, 0x2028: true, 0x2029: true,
}

// recalled shows recalled memories, and a memory that could not be read.
// done prints the turn's cost, workers included.
func (c *cli) done(ev agent.Event) error {
	u := ev.Result.Usage
	workers := ""
	if in, out := c.workerIn.Swap(0), c.workerOut.Swap(0); in+out > 0 {
		workers = fmt.Sprintf("; workers %d in / %d out", in, out)
	}
	return c.line(fmt.Sprintf("\n  [%s, %d model calls, %d in / %d out tokens%s]", ev.Stop, ev.Result.Turns, u.Input, u.Output, workers))
}

func (c *cli) toolEnd(ev agent.Event) error {
	if ev.Call.Name == todo.ToolName && !ev.Output.IsError {
		return c.showTodos(ev.Call)
	}
	mark := "<-"
	if ev.Output.IsError {
		mark = "<- error:"
	}
	return c.line(fmt.Sprintf("  %s %s", mark, short(ev.Output.Content)))
}

func (c *cli) recalled(ev agent.Event) error {
	switch {
	case errors.HasCode(ev.Err, agent.ErrMemoryPartial):
		return c.line("  * recalled " + ev.Text + " (by words only: " + describe(ev.Err) + ")")
	case ev.Err != nil:
		return c.line("  ! memory unavailable: " + describe(ev.Err))
	case ev.Text != "0 memories":
		return c.line("  * recalled " + ev.Text)
	}
	return nil
}
