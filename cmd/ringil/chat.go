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
	return closing(c.turn(ctx.Context(), a, conv, prompt), release)
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

func (c *cli) chat(ctx context.Context, a agent.Agent, conv agent.Conversation) error {
	for c.prompt() {
		raw, ok := c.readLine()
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
			if err := c.turn(ctx, a, conv, line); err != nil {
				c.note("error: " + describe(err))
			}
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
	ctx, cancel := c.turnContext(ctx)
	defer cancel()
	for ev, err := range a.Stream(ctx, conv, agent.UserText(prompt)) {
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
	case agent.EventCompacted:
		err = c.line("  ~ context compacted: " + ev.Text)
	case agent.EventRecovered:
		err = c.line("  ! " + ev.Call.Name + " was interrupted earlier; the model was told, it was not run again")
	case agent.EventDone:
		u := ev.Result.Usage
		err = c.line(fmt.Sprintf("\n  [%s, %d model calls, %d in / %d out tokens]", ev.Stop, ev.Result.Turns, u.Input, u.Output))
	}
	return err
}

func (c *cli) line(s string) error {
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
	switch {
	case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
		return false
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f:
		return false
	}
	return true
}

// recalled shows recalled memories, and a memory that could not be read.
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
