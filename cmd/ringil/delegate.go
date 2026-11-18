// delegate.go: sub-agents in the terminal agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/notes"
	"github.com/agilira/ringil-agent-sdk/todo"
)

const exploreSystem = "You are a research sub-agent. Search and read to answer the task you are given, then report what you found: concrete facts, file paths and line numbers, and what you could not establish. Do not guess. Your report is all the asking agent will see."

const generalSystem = "You are a sub-agent working on one task for another agent. Do the task with your tools, then report concisely what you did, what you found and anything left open. Your report is all the asking agent will see."

// delegateTool gives the agent two kinds of worker: explore, read-only,
// for searches that would fill its context, and general, with the same
// tools as the parent but no delegation of its own. Each task gets a
// scratchpad of its own and reads the project notes and rules.
func (c *cli) delegateTool(ctx *orpheus.Context, p agent.Provider, parentTools agent.Registry) (agent.Tool, error) {
	explore, err := agent.NewRegistry(c.readTools...)
	if err != nil {
		return nil, err
	}
	general, err := sharedTools(parentTools)
	if err != nil {
		return nil, err
	}
	crew, err := delegate.NewCrew(delegate.Team{
		Provider: p,
		Approve:  c.approve,
		MaxTurns: ctx.GetGlobalFlagInt("max-turns"),
		OnEvent:  c.showWorker,
		Workers: []delegate.Worker{
			{Name: "explore", Description: "searches and reads files, read-only; for questions like where is X, how does Y work", System: exploreSystem, Tools: explore, Prepare: c.prepareWorker},
			{Name: "general", Description: "does a self-contained task with the same tools as you, except delegating and your own notes, task list and memory", System: generalSystem, Tools: general, Prepare: c.prepareWorker},
		},
	})
	if err != nil {
		return nil, err
	}
	c.crew = crew
	c.closers = append(c.closers, func() error { crew.Close(); return nil })
	return crew.Tool(), nil
}

// personal are the parent's own: its notebooks, task list and memory. A
// worker gets its own scratchpad and list instead, and no memory.
var personal = map[string]bool{notes.DefaultName: true, todo.ToolName: true, "remember": true, "recall": true, "forget": true,
	"list_artifacts": true, "read_artifact": true, "publish_artifact": true}

func sharedTools(parent agent.Registry) (agent.Registry, error) {
	var names []string
	for _, s := range parent.Specs() {
		if !personal[s.Name] {
			names = append(names, s.Name)
		}
	}
	return parent.Select(names...)
}

// prepareWorker gives a task its own scratchpad, the project notes read
// only, and the project rules.
func (c *cli) prepareWorker(context.Context, string) (delegate.Setup, error) {
	dir, err := os.MkdirTemp("", "ringil-worker-")
	if err != nil {
		return delegate.Setup{}, err
	}
	scopes := []notes.Scope{{Name: "scratchpad", Dir: filepath.Join(dir, "scratch")}}
	if c.projectNotes != "" {
		scopes = append(scopes, notes.Scope{Name: "project", Dir: c.projectNotes, ReadOnly: true})
	}
	nb, err := notes.New(scopes...)
	if err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			return delegate.Setup{}, rerr
		}
		return delegate.Setup{}, err
	}
	return delegate.Setup{
		Tools:   append([]agent.Tool{nb.Tool(), todo.Tool()}, c.artifactTools("worker")...),
		Context: c.runContext(nb),
		Done:    func() error { return os.RemoveAll(dir) },
	}, nil
}

// showWorker prints what a worker does, indented under the parent's call.
func (c *cli) showWorker(task int, worker string, ev agent.Event) {
	tag := "    [" + worker + " " + strconv.Itoa(task) + "] "
	switch ev.Kind {
	case agent.EventToolStart:
		c.note(strings.TrimPrefix(tag, "  ") + "-> " + ev.Call.Name + " " + short(string(ev.Call.Input)))
	case agent.EventDone:
		c.workerIn.Add(int64(ev.Result.Usage.Input))
		c.workerOut.Add(int64(ev.Result.Usage.Output))
		c.note(strings.TrimPrefix(tag, "  ") + "done, " + strconv.Itoa(ev.Result.Turns) + " model calls")
	}
}
