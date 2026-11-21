// memory.go: what the agent remembers across conversations
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/memory/chroma"
	"github.com/agilira/ringil-agent-sdk/memory/mem0"
	"github.com/agilira/ringil-agent-sdk/memory/sqlite"
	"github.com/agilira/ringil-agent-sdk/provider/ollama"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/secrets"
)

// memory opens the user's memory, or returns nil with --no-memory. It is on
// by default: a complete agent remembers what the user told it. It lives
// in a SQLite database in the config directory, or on a Chroma or Mem0
// server (memory_store); with an embedding model it also finds memories by
// meaning (Mem0 always does, with its own). The database is closed when the run ends.
func (c *cli) memory(ctx *orpheus.Context) (agent.Memory, error) {
	if c.flagBool(ctx, "no-memory") {
		return nil, nil
	}
	base, err := c.configDir()
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "no user config directory; use --no-memory")
	}
	dir, ok := c.memoryDir(filepath.Join(base, "ringil", "memory"))
	if !ok {
		return nil, nil
	}
	store := c.conf().Memory
	if store.Kind == "mem0" {
		return c.mem0Memory(ctx, store)
	}
	emb, err := c.embedder(ctx)
	if err != nil {
		return nil, err
	}
	if store.Kind == "chroma" {
		return c.chromaMemory(ctx, store, emb)
	}
	m, err := sqlite.New(dir, emb)
	if err != nil {
		return nil, err
	}
	c.closers = append(c.closers, m.Close)
	return m, nil
}

// chatCollection is a chat's collection, or Mem0 user: <prefix>-chat-<chat>,
// or, for a chat whose name the server would refuse (valid), the same with
// a hash of it.
func chatCollection(prefix, chat string, valid func(string) bool) string {
	if name := prefix + "-chat-" + chat; valid(name) {
		return name
	}
	sum := sha256.Sum256([]byte(chat))
	return prefix + "-chat-" + hex.EncodeToString(sum[:8])
}

// chromaMemory is the memory memoryDir chose (the user's, or a chat's) in
// a Chroma collection of its own.
func (c *cli) chromaMemory(ctx *orpheus.Context, store memoryStore, emb agent.Embedder) (agent.Memory, error) {
	if emb == nil {
		return nil, errors.New(ErrUsage, "memory_store chroma finds memories by meaning: set embed_model")
	}
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	name := store.prefix() + "-user"
	if c.role != nil && c.role.Memory == "chat" { // memoryDir made sure of the chat
		name = chatCollection(store.prefix(), channel.ChatOf(c.runConversation), chroma.ValidCollection)
	}
	return chroma.New(ctx.Context(), chroma.Config{URL: store.URL, Tenant: store.Tenant, Database: store.Database,
		Collection: name, Embedder: emb, Headers: headerFunc(store.Headers, res), AllowHTTP: c.flagBool(ctx, "allow-http")})
}

// mem0Memory is the memory memoryDir chose (the user's, or a chat's)
// under a Mem0 user_id of its own. The vectors are the server's: Ringil's
// embedder is not used.
func (c *cli) mem0Memory(ctx *orpheus.Context, store memoryStore) (agent.Memory, error) {
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	name := store.prefix() + "-user"
	if c.role != nil && c.role.Memory == "chat" { // memoryDir made sure of the chat
		name = chatCollection(store.prefix(), channel.ChatOf(c.runConversation), mem0.ValidUser)
	}
	return mem0.New(ctx.Context(), mem0.Config{URL: store.URL, User: name, Raw: store.Raw,
		Headers: headerFunc(store.Headers, res), AllowHTTP: c.flagBool(ctx, "allow-http")})
}

// memoryDir is where the role remembers: the user's memory, or a chat's
// own (the chat of the run's conversation); not ok for none.
func (c *cli) memoryDir(user string) (string, bool) {
	if c.role == nil || c.role.Memory == "" {
		return user, true
	}
	chat := channel.ChatOf(c.runConversation)
	if c.role.Memory == "off" || chat == "" {
		return "", false // without a chat, nothing of its own to remember
	}
	return filepath.Join(user, "chats", chat), true
}

// embedder is the embedder of ringil.yaml when it names one (ollama),
// else the embedding model on the same endpoint and key as the chat model
// (the role's, under a role with its own base), or nil when none is set.
func (c *cli) embedder(ctx *orpheus.Context) (agent.Embedder, error) {
	if e := c.conf().Embedder; e.Kind == "ollama" {
		return ollama.NewEmbedder(ollama.Config{URL: e.URL, Model: e.Model, QueryPrefix: e.QueryPrefix,
			DocumentPrefix: e.DocumentPrefix, Dimensions: e.Dimensions, AllowHTTP: c.flagBool(ctx, "allow-http")})
	}
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	model, err := c.embedModel(ctx, res)
	if err != nil || model == "" {
		return nil, err
	}
	return c.endpointEmbedder(ctx, res, model)
}

// endpointEmbedder is model at the chat model's endpoint, with its key.
func (c *cli) endpointEmbedder(ctx *orpheus.Context, res secrets.Resolver, model string) (agent.Embedder, error) {
	switch kind, err := c.providerKind(ctx, res); {
	case err != nil:
		return nil, err
	case kind == providerAnthropic:
		return nil, errors.New(ErrUsage, "Anthropic has no embedding models: set embedder kind ollama, or no embed_model")
	}
	base, err := c.roleSetting(ctx, res, "base", "RINGIL_BASE_URL")
	if err != nil {
		return nil, err
	}
	keyName, err := c.keyName(ctx, res)
	if err != nil {
		return nil, err
	}
	return c.newEmbedder(openai.Config{
		BaseURL:   base,
		Model:     model,
		KeyFunc:   keyFunc(res, keyName),
		AllowHTTP: c.flagBool(ctx, "allow-http"),
		// Asymmetric models (NVIDIA NIM) need input_type.
		EmbedInputType: c.getenv("RINGIL_EMBED_INPUT_TYPE") == "1" || c.conf().EmbedInputType,
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
		c.note(err.Error()) // which part worked, in the memory's words
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
