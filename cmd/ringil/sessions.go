// sessions.go: saved conversations
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"fmt"
	"path/filepath"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/session/file"
)

func (c *cli) store(ctx *orpheus.Context) (agent.Store, error) {
	dir, err := c.sessionsDir(ctx)
	if err != nil {
		return nil, err
	}
	return file.New(dir)
}

func (c *cli) sessionsDir(ctx *orpheus.Context) (string, error) {
	if dir := c.flagString(ctx, "sessions-dir"); dir != "" {
		return filepath.Abs(dir)
	}
	base, err := c.configDir()
	if err != nil {
		return "", errors.Wrap(err, ErrUsage, "no user config directory; use --sessions-dir")
	}
	return filepath.Join(base, "ringil", "sessions"), nil
}

// conversation returns the conversation for this invocation and a function
// that releases it. Without --session it lives in memory only.
func (c *cli) conversation(ctx *orpheus.Context) (agent.Conversation, func() error, error) {
	id := ctx.GetGlobalFlagString("session")
	if id == "" {
		return agent.NewConversation(), func() error { return nil }, nil
	}
	store, err := c.store(ctx)
	if err != nil {
		return nil, nil, err
	}
	sess, err := store.Open(ctx.Context(), id)
	if err != nil {
		return nil, nil, err
	}
	if n := len(sess.Messages()); n > 0 {
		c.note(fmt.Sprintf("resumed session %s (%d messages)", id, n))
	}
	return sess, sess.Close, nil
}

func (c *cli) cmdSessions(ctx *orpheus.Context) error {
	store, err := c.store(ctx)
	if err != nil {
		return err
	}
	list, err := store.List(ctx.Context())
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return c.line("no saved sessions")
	}
	for _, s := range list {
		if err := c.line(fmt.Sprintf("%-24s %s  %7d bytes", s.ID, s.Updated.Format("2006-01-02 15:04"), s.Size)); err != nil {
			return err
		}
	}
	return nil
}

func (c *cli) cmdSessionRemove(ctx *orpheus.Context) error {
	ids := ctx.Positional()
	if len(ids) == 0 {
		return errors.New(ErrUsage, "name the sessions to delete")
	}
	store, err := c.store(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := store.Delete(ctx.Context(), id); err != nil {
			return err
		}
		if err := c.removeScratch(ctx, id); err != nil {
			return err
		}
		c.note("deleted " + id)
	}
	return nil
}
