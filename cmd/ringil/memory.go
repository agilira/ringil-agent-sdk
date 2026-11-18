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
	"github.com/agilira/ringil-agent-sdk/memory/vector"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/secrets"
)

// memory opens the user's memory, or returns nil with --no-memory. It is on
// by default: a complete agent remembers what the user told it. With an
// embedding model (--embed-model or RINGIL_EMBED_MODEL) it also finds
// memories by meaning; the records are the same either way.
func (c *cli) memory(ctx *orpheus.Context) (agent.Memory, error) {
	if ctx.GetGlobalFlagBool("no-memory") {
		return nil, nil
	}
	base, err := c.configDir()
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "no user config directory; use --no-memory")
	}
	dir := filepath.Join(base, "ringil", "memory")
	emb, err := c.embedder(ctx)
	if err != nil || emb == nil {
		if err != nil {
			return nil, err
		}
		return local.New(dir)
	}
	return vector.New(dir, emb)
}

// embedder is the embedding model on the same endpoint and key as the
// chat model, or nil when none is set.
func (c *cli) embedder(ctx *orpheus.Context) (agent.Embedder, error) {
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	model, err := c.setting(ctx, res, "embed-model", "RINGIL_EMBED_MODEL")
	if err != nil || model == "" {
		return nil, err
	}
	base, err := c.setting(ctx, res, "base", "RINGIL_BASE_URL")
	if err != nil {
		return nil, err
	}
	if err := secrets.ValidateName(ctx.GetGlobalFlagString("key-env")); err != nil {
		return nil, err
	}
	return c.newEmbedder(openai.Config{
		BaseURL:   base,
		Model:     model,
		KeyFunc:   secrets.OptionalKeyFunc(res, ctx.GetGlobalFlagString("key-env")),
		AllowHTTP: ctx.GetGlobalFlagBool("allow-http"),
		// Asymmetric models (NVIDIA NIM) need input_type.
		EmbedInputType: c.getenv("RINGIL_EMBED_INPUT_TYPE") == "1",
	})
}

func (c *cli) cmdMemory(ctx *orpheus.Context) error {
	m, err := c.memory(ctx)
	if err != nil || m == nil {
		return err
	}
	recs, err := m.Search(ctx.Context(), strings.Join(ctx.Positional(), " "), 1000)
	switch {
	case errors.HasCode(err, agent.ErrMemoryPartial):
		c.note("searching by words only: " + err.Error())
	case err != nil:
		return err
	}
	if len(recs) == 0 {
		return c.line("nothing remembered")
	}
	for _, r := range recs {
		if err := c.line(fmt.Sprintf("%s  %s  %s", clean(r.ID), r.Created.Format("2006-01-02"), short(r.Content))); err != nil {
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
