// tool.go: check_later and stop_checking
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package later

import (
	"context"
	"strconv"
	"strings"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Tool names.
const (
	CheckTool = "check_later"
	StopTool  = "stop_checking"
)

type checkIn struct {
	What  string `json:"what" desc:"What to look at and what to do when it is done, as you would tell yourself: the download in ~/Downloads/x.iso; when it is complete, verify its checksum"`
	Every string `json:"every" desc:"How long to wait between checks: 30s, 5m, 1h (at least the host's minimum, usually 1m)"`
	For   string `json:"for,omitempty" desc:"How long to keep checking at most: 2h (default 1h, at most 24h)"`
	Times int    `json:"times,omitempty" desc:"Most checks to run (default: as many as fit)"`
}

type stopIn struct {
	ID int `json:"id" desc:"The check's id, as check_later gave it"`
}

// defaultSpan is how long a check goes on when the agent does not say.
const defaultSpan = time.Hour

// Tools returns check_later and stop_checking on board b. conv gives the
// host's key for the conversation a call belongs to; "" refuses the call
// (no conversation the host can come back to). Hosts offer them only
// where a check can come back: a chat, not a one-off API call.
func Tools(b *Board, conv func(context.Context) string) []agent.Tool {
	check := agent.NewTool(CheckTool,
		"Come back to something later in this conversation: a download, a build, a reply you wait for. "+
			"Each check is a turn of yours here; you stop it with stop_checking when it is done. "+
			"For the task at hand only: what should happen by itself from now on is an automation, which the user sets up with /automate.",
		func(ctx context.Context, in checkIn) (string, error) {
			every, span, err := durations(in.Every, in.For)
			if err != nil {
				return "", err
			}
			c, err := b.Add(conv(ctx), in.What, every, span, in.Times)
			if err != nil {
				return "", err
			}
			return "Check " + strconv.Itoa(c.ID) + " set: every " + every.String() + ", at most " + strconv.Itoa(c.Max) +
				" times, until " + c.Until.Format("15:04") + ". Each check comes to you as a message; stop it with " + StopTool + " when it is done.", nil
		})
	stop := agent.NewTool(StopTool,
		"Stop a check set with check_later: what it waited for is done, or the user no longer wants it.",
		func(ctx context.Context, in stopIn) (string, error) {
			if err := b.Stop(conv(ctx), in.ID); err != nil {
				return "", agent.ToolError(agent.ErrToolInput, agent.ModelMessage(err)+openList(b.Of(conv(ctx))))
			}
			return "Check " + strconv.Itoa(in.ID) + " stopped.", nil
		})
	return []agent.Tool{check, stop}
}

func durations(every, span string) (time.Duration, time.Duration, error) {
	e, err := time.ParseDuration(strings.TrimSpace(every))
	if err != nil {
		return 0, 0, agent.ToolError(agent.ErrToolInput, "every is a duration like 5m or 1h")
	}
	s := defaultSpan
	if strings.TrimSpace(span) != "" {
		if s, err = time.ParseDuration(strings.TrimSpace(span)); err != nil {
			return 0, 0, agent.ToolError(agent.ErrToolInput, "for is a duration like 2h")
		}
	}
	return e, s, nil
}

func openList(list []Check) string {
	if len(list) == 0 {
		return " (no check is open)"
	}
	ids := make([]string, len(list))
	for i, c := range list {
		ids[i] = strconv.Itoa(c.ID)
	}
	return " (open: " + strings.Join(ids, ", ") + ")"
}
