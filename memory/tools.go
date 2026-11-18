// tools.go: tools that let the model manage its own memory
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package memory gives an agent tools over an agent.Memory: remember,
// recall and forget. Backends are in sub-packages (memory/local).
//
// Granting these tools lets the model write what later conversations will
// see. What it stores is still recalled as reference data, never as
// instructions, so a fact planted through a hostile document cannot later
// pose as the user's or the host's wish.
package memory

import (
	"context"
	"strings"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

type rememberIn struct {
	Content string `json:"content" desc:"One fact worth keeping across conversations: a preference, a decision, a name, a deadline"`
}

type recallIn struct {
	Query string `json:"query,omitempty" desc:"Words to look for; empty lists the most recent"`
	Limit int    `json:"limit,omitempty" min:"1" max:"100" desc:"How many; default 10"`
}

type forgetIn struct {
	ID string `json:"id" desc:"ID of the memory, as shown by recall"`
}

// Tools returns remember, recall and forget over m.
func Tools(m agent.Memory) []agent.Tool {
	return []agent.Tool{
		agent.NewTool("remember", "Keep a fact for future conversations. Store facts the user would expect you to know next time; one fact per call.",
			func(ctx context.Context, in rememberIn) (string, error) {
				rec, err := m.Put(ctx, agent.MemoryRecord{Content: in.Content})
				if err != nil {
					return "", err
				}
				return "Remembered as " + rec.ID + ".", nil
			}),
		agent.NewTool("recall", "Search what you remembered in earlier conversations.",
			func(ctx context.Context, in recallIn) (string, error) {
				limit := in.Limit
				if limit == 0 {
					limit = 10
				}
				recs, err := m.Search(ctx, in.Query, limit)
				if err != nil && !errors.HasCode(err, agent.ErrMemoryPartial) {
					return "", err
				}
				return format(recs), nil
			}, agent.ReadOnly()),
		agent.NewTool("forget", "Delete a remembered fact that is wrong or no longer wanted.",
			func(ctx context.Context, in forgetIn) (string, error) {
				if err := m.Delete(ctx, in.ID); err != nil {
					return "", err
				}
				return "Forgotten.", nil
			}, agent.Destructive()),
	}
}

func format(recs []agent.MemoryRecord) string {
	if len(recs) == 0 {
		return "Nothing remembered matches."
	}
	var b strings.Builder
	for _, r := range recs {
		b.WriteString("[" + r.ID + "] " + r.Content + " (" + r.Created.Format("2006-01-02") + ")\n")
	}
	return b.String()
}
