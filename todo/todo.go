// todo.go: the agent's own task list
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package todo gives the model a task list it keeps for long work: it
// writes the whole list each time, with one item in progress, and marks
// items done as it goes. The list lives in the conversation itself (the
// arguments of the last todo_write call), so it is saved with the session
// and needs no store; Current reads it back for a host that shows it.
package todo

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	agent "github.com/agilira/ringil-agent-sdk"
)

// ToolName is the name of the tool that writes the list.
const ToolName = "todo_write"

// Statuses of an item.
const (
	Pending    = "pending"
	InProgress = "in_progress"
	Completed  = "completed"
)

// Item is one task.
type Item struct {
	Content    string `json:"content" desc:"What to do, in the imperative: Run the tests"`
	Status     string `json:"status" enum:"pending,in_progress,completed" desc:"pending, in_progress (exactly one while you work) or completed"`
	ActiveForm string `json:"active_form,omitempty" desc:"The same in the present continuous, shown while in progress: Running the tests"`
}

type writeIn struct {
	Todos []Item `json:"todos" desc:"The whole list, in order; it replaces the previous one"`
}

// maxItems bounds a list; a task needing more is several tasks.
const maxItems = 100

// Tool returns todo_write.
func Tool() agent.Tool {
	return agent.NewTool(ToolName,
		"Keep a task list for work with several steps: write the whole list, mark one item in_progress before starting it and completed as soon as it is done. Skip it for a single quick step.",
		func(_ context.Context, in writeIn) (string, error) {
			if err := check(in.Todos); err != nil {
				return "", err
			}
			return summary(in.Todos), nil
		}, agent.ReadOnly())
}

func check(items []Item) error {
	if len(items) > maxItems {
		return agent.ToolError(agent.ErrToolInput, "Keep the list under "+strconv.Itoa(maxItems)+" items.")
	}
	active := 0
	for _, it := range items {
		if strings.TrimSpace(it.Content) == "" {
			return agent.ToolError(agent.ErrToolInput, "Every item needs content.")
		}
		if it.Status == InProgress {
			active++
		}
	}
	if active > 1 {
		return agent.ToolError(agent.ErrToolInput, "Only one item can be in_progress at a time.")
	}
	return nil
}

func summary(items []Item) string {
	done := 0
	for _, it := range items {
		if it.Status == Completed {
			done++
		}
	}
	return "Task list saved: " + strconv.Itoa(done) + " of " + strconv.Itoa(len(items)) + " done."
}

// Current returns the list as last written in the conversation, or nil.
// Only calls whose result was not an error count.
func Current(history []agent.Message) []Item {
	ok := map[string]bool{}
	for _, m := range history {
		if m.Result != nil && !m.Result.IsError {
			ok[m.Result.CallID] = true
		}
	}
	var last []Item
	for _, m := range history {
		for _, c := range m.Calls {
			if c.Name != ToolName || !ok[c.ID] {
				continue
			}
			var in writeIn
			if json.Unmarshal(c.Input, &in) == nil {
				last = in.Todos
			}
		}
	}
	return last
}
