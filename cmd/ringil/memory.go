// memory.go: what the agent remembers across conversations
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/memory/local"
)

// memory opens the user's memory, or returns nil with --no-memory. It is on
// by default: a complete agent remembers what the user told it.
func (c *cli) memory(ctx *orpheus.Context) (agent.Memory, error) {
	if ctx.GetGlobalFlagBool("no-memory") {
		return nil, nil
	}
	base, err := c.configDir()
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "no user config directory; use --no-memory")
	}
	return local.New(filepath.Join(base, "ringil", "memory"))
}

func (c *cli) cmdMemory(ctx *orpheus.Context) error {
	m, err := c.memory(ctx)
	if err != nil || m == nil {
		return err
	}
	recs, err := m.Search(ctx.Context(), strings.Join(ctx.Positional(), " "), 1000)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return c.line("nothing remembered")
	}
	for _, r := range recs {
		if err := c.line(fmt.Sprintf("%s  %s  %s", r.ID, r.Created.Format("2006-01-02"), clean(r.Content))); err != nil {
			return err
		}
	}
	return nil
}

func (c *cli) cmdMemoryRemove(ctx *orpheus.Context) error {
	ids := ctx.Positional()
	if len(ids) == 0 {
		return errors.New(ErrUsage, "name the memories to delete (see: ringil memory)")
	}
	m, err := c.memory(ctx)
	if err != nil || m == nil {
		return err
	}
	for _, id := range ids {
		if err := m.Delete(ctx.Context(), id); err != nil {
			return err
		}
		c.note("forgot " + id)
	}
	return nil
}
