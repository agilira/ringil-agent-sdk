// chat.go: the interactive chat and the one-shot run
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
)

// maxLine bounds one line typed at the prompt.
const maxLine = 1 << 20

func (c *cli) cmdRun(ctx *orpheus.Context) error {
	prompt := strings.TrimSpace(strings.Join(ctx.Positional(), " "))
	if prompt == "" {
		return errors.New(ErrUsage, `nothing to send; usage: ringil run "<prompt>"`)
	}
	a, err := c.agent(ctx)
	if err != nil {
		return err
	}
	_, err = c.turn(ctx.Context(), a, nil, prompt)
	return err
}

func (c *cli) cmdChat(ctx *orpheus.Context) error {
	a, err := c.agent(ctx)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(c.in)
	scanner.Buffer(make([]byte, 0, 4096), maxLine)
	var history []agent.Message
	for c.prompt() && scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "":
			continue
		case "/exit", "/quit":
			return nil
		case "/reset":
			history = nil
			c.note("history cleared")
			continue
		}
		history = c.chatTurn(ctx.Context(), a, history, line)
	}
	return scanner.Err()
}

// chatTurn runs one prompt; a failed turn is reported and the chat goes on
// with the history it had before.
func (c *cli) chatTurn(ctx context.Context, a agent.Agent, history []agent.Message, line string) []agent.Message {
	added, err := c.turn(ctx, a, history, line)
	if err != nil {
		c.note("error: " + describe(err))
		return history
	}
	return append(history, added...)
}

func (c *cli) prompt() bool {
	_, err := fmt.Fprint(c.out, "\n> ")
	return err == nil
}

// turn sends one prompt and renders its events.
func (c *cli) turn(ctx context.Context, a agent.Agent, history []agent.Message, prompt string) ([]agent.Message, error) {
	ctx, cancel := c.turnContext(ctx)
	defer cancel()
	for ev, err := range a.Stream(ctx, history, agent.UserText(prompt)) {
		if err != nil {
			return nil, err
		}
		if err := c.render(ev); err != nil {
			return nil, err
		}
		if ev.Kind == agent.EventDone {
			return ev.Result.Messages, nil
		}
	}
	return nil, context.Canceled
}

func (c *cli) render(ev agent.Event) error {
	var err error
	switch ev.Kind {
	case agent.EventText:
		_, err = fmt.Fprint(c.out, clean(ev.Text))
	case agent.EventToolStart:
		err = c.line(fmt.Sprintf("\n  -> %s %s", ev.Call.Name, short(string(ev.Call.Input))))
	case agent.EventToolEnd:
		mark := "<-"
		if ev.Output.IsError {
			mark = "<- error:"
		}
		err = c.line(fmt.Sprintf("  %s %s", mark, short(ev.Output.Content)))
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

// clean removes control characters except newlines and tabs. Model output
// and tool results are untrusted: an escape sequence printed raw could
// rewrite the terminal, fake a prompt or hide text.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f)) {
			return r
		}
		return -1
	}, s)
}
