// notes.go: the agent's task list and notebooks in the terminal
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/notes"
	"github.com/agilira/ringil-agent-sdk/todo"
)

// notebooks are a scratchpad for this session (kept with a --session,
// thrown away at exit without one) and the notes of the project in the
// start directory, kept across sessions.
func (c *cli) notebooks(ctx *orpheus.Context) (*notes.Notes, error) {
	if ctx.GetGlobalFlagBool("no-tools") {
		return nil, nil
	}
	base, err := c.configDir()
	if err != nil {
		return nil, nil // no config directory: no notebooks, the agent still works
	}
	root := filepath.Join(base, "ringil")
	scratch, err := c.scratchDir(ctx, root)
	if err != nil {
		return nil, err
	}
	cwd, err := c.cwd()
	if err != nil {
		return nil, err
	}
	project := filepath.Join(root, "projects", projectKey(cwd), "notes")
	c.projectNotes = project
	// Project notes last across sessions: a change there is confirmed.
	return notes.New(notes.Scope{Name: "scratchpad", Dir: scratch}, notes.Scope{Name: "project", Dir: project, ConfirmChanges: true})
}

// scratchDir is the session's scratchpad, keyed by sessions directory and
// ID (two stores may hold the same ID), or one for this run, removed at
// exit; run scratchpads left by a crash are cleared after a week.
func (c *cli) scratchDir(ctx *orpheus.Context, root string) (string, error) {
	scratch := filepath.Join(root, "scratch")
	if id := ctx.GetGlobalFlagString("session"); id != "" {
		return c.sessionScratch(ctx, scratch, id)
	}
	clearStale(scratch)
	dir := filepath.Join(scratch, "run-"+strings.ToLower(rand.Text()[:12]))
	c.closers = append(c.closers, func() error { return os.RemoveAll(dir) })
	return dir, nil
}

func (c *cli) sessionScratch(ctx *orpheus.Context, scratch, id string) (string, error) {
	if err := agent.ValidateSessionID(id); err != nil {
		return "", err
	}
	sdir, err := c.sessionsDir(ctx)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sdir))
	return filepath.Join(scratch, hex.EncodeToString(sum[:4])+"-"+id), nil
}

// removeScratch deletes a session's scratchpad with the session.
func (c *cli) removeScratch(ctx *orpheus.Context, id string) error {
	base, err := c.configDir()
	if err != nil {
		return nil
	}
	dir, err := c.sessionScratch(ctx, filepath.Join(base, "ringil", "scratch"), id)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// clearStale removes run scratchpads older than a week: a run killed
// before its exit could not remove its own.
func clearStale(scratch string) {
	entries, err := os.ReadDir(scratch)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil || !e.IsDir() || !strings.HasPrefix(e.Name(), "run-") || time.Since(info.ModTime()) < 7*24*time.Hour {
			continue
		}
		if rerr := os.RemoveAll(filepath.Join(scratch, e.Name())); rerr != nil {
			continue // left for the next start
		}
	}
}

// projectKey names a project's notes after its directory: readable, and
// unique through a short hash of the full path.
func projectKey(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, filepath.Base(dir))
	return clipName(name, 40) + "-" + hex.EncodeToString(sum[:4])
}

func clipName(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// showTodos prints the task list each time the agent writes it.
func (c *cli) showTodos(call *agent.ToolCall) error {
	var in struct {
		Todos []todo.Item `json:"todos"`
	}
	if json.Unmarshal(call.Input, &in) != nil {
		return nil
	}
	for _, it := range in.Todos {
		mark, text := "[ ]", it.Content
		switch it.Status {
		case todo.InProgress:
			mark = "[>]"
			if it.ActiveForm != "" {
				text = it.ActiveForm
			}
		case todo.Completed:
			mark = "[x]"
		}
		if err := c.line("  " + mark + " " + short(text)); err != nil {
			return err
		}
	}
	return nil
}
