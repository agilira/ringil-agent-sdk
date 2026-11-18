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

	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/notes"
	"github.com/agilira/ringil-agent-sdk/todo"
)

// notebooks are a scratchpad for this session (kept with a --session,
// thrown away at exit without one) and the notes of the project in the
// start directory, kept across sessions.
func (c *cli) notebooks(ctx *orpheus.Context) (*notes.Notes, error) {
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
	return notes.New(notes.Scope{Name: "scratchpad", Dir: scratch}, notes.Scope{Name: "project", Dir: project})
}

func (c *cli) scratchDir(ctx *orpheus.Context, root string) (string, error) {
	if id := ctx.GetGlobalFlagString("session"); id != "" {
		return filepath.Join(root, "scratch", id), nil
	}
	dir := filepath.Join(root, "scratch", "run-"+strings.ToLower(rand.Text()[:12]))
	c.closers = append(c.closers, func() error { return os.RemoveAll(dir) })
	return dir, nil
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
