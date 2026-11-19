// tool.go: create_trigger, triggers the agent makes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// ErrExists: a trigger by that name is there already.
const ErrExists errors.ErrorCode = "TRIGGER_EXISTS"

// Add adds a trigger, refusing one whose name is taken.
func (s *Store) Add(t Trigger) error {
	if _, err := t.Check(); err != nil {
		return err
	}
	return s.editTriggers(func(triggers []Trigger) ([]Trigger, error) {
		for _, old := range triggers {
			if old.Name == t.Name {
				return nil, errors.New(ErrExists, "a trigger "+t.Name+" exists already")
			}
		}
		if len(triggers) >= MaxTriggers {
			return nil, errors.New(ErrConfig, "the store holds at most "+strconv.Itoa(MaxTriggers)+" triggers")
		}
		return append(triggers, t), nil
	})
}

// ToolOptions set up create_trigger.
type ToolOptions struct {
	// Defaults fills what the agent leaves out (the chat to deliver to,
	// the role that runs it), from the run that makes the trigger.
	Defaults func(ctx context.Context, t *Trigger)
	// Check is the host's say on a trigger, after Defaults: the tools it
	// names exist, the path it watches may be watched, the chat it
	// listens to is this one. An error refuses it.
	Check func(ctx context.Context, t Trigger) error
	// Added hears of each trigger added (a running Runner's Reload).
	Added func(Trigger)
}

type createIn struct {
	Name    string          `json:"name" desc:"A name: 1-48 letters, digits, - and _"`
	Kind    Kind            `json:"kind" desc:"What sets it off: time, tool (a tool's answer changes), file (files change)"`
	When    string          `json:"when,omitempty" desc:"time: cron (\"0 8 * * 1-5\"), @daily, @hourly, @weekly or @every 30m; tool and file: how often to look (default @every 5m and @every 1m)"`
	Zone    string          `json:"time_zone,omitempty" desc:"IANA time zone for when, e.g. Europe/Rome; default the host's"`
	Tool    string          `json:"tool,omitempty" desc:"tool: the tool to ask, one that only reads"`
	Args    json.RawMessage `json:"args,omitempty" desc:"tool: its arguments, the same every time"`
	Path    string          `json:"path,omitempty" desc:"file: the absolute path of the file or directory to watch"`
	Skill   string          `json:"skill,omitempty" desc:"The skill each run follows (make it first with create_skill if needed)"`
	Prompt  string          `json:"prompt,omitempty" desc:"What each run is told to do, besides the skill"`
	Allow   []string        `json:"allow,omitempty" desc:"Tools the runs may use without asking (no one is there to ask): only what the task needs; anything else that would ask is refused"`
	Timeout string          `json:"timeout,omitempty" desc:"Stop a run that takes longer, from 1m to 2h; default 10m"`
	Keep    bool            `json:"continue,omitempty" desc:"Carry on one conversation across runs (each run remembers the last)"`
}

// createKeys are the only fields create_trigger takes, exactly: what the
// user is shown is what is made, so no field is dropped or read another
// way (a "Name" beside a "name").
var createKeys = map[string]bool{"name": true, "kind": true, "when": true, "time_zone": true, "tool": true, "args": true,
	"path": true, "skill": true, "prompt": true, "allow": true, "timeout": true, "continue": true}

// CreateTool is create_trigger: the agent makes a trigger in the store
// open returns, which runs while the host runs the triggers (`ringil
// serve`). It is for automations that last: what the user wants done by
// itself from now on, not a check during the task at hand. It is marked
// destructive: the host asks the user once, showing what will set it off,
// what it runs and what it may do without asking; its runs then ask no
// one. Webhooks and message triggers are made by the user.
func CreateTool(open func() (*Store, error), opts ToolOptions) agent.Tool {
	inner := agent.NewTool("create_trigger",
		"Make a lasting automation: a run of yours that sets off by itself from now on, at a time, when a tool's answer changes, or when files change, "+
			"and goes on after this conversation. Only when the user asks for something to happen by itself from now on (every day, whenever...). "+
			"Not for checking on something during the task at hand. Its runs have no one to ask: give in allow only the tools they need.",
		func(ctx context.Context, in createIn) (string, error) {
			t := in.trigger()
			if opts.Defaults != nil {
				opts.Defaults(ctx, &t)
			}
			if err := refused(t); err != nil {
				return "", err
			}
			if opts.Check != nil {
				if err := opts.Check(ctx, t); err != nil {
					return "", err
				}
			}
			store, err := open()
			if err != nil {
				return "", err
			}
			err = store.Add(t)
			_ = store.Close()
			if err != nil {
				return "", err
			}
			if opts.Added != nil {
				opts.Added(t)
			}
			return "Trigger " + t.Name + " made: it runs while the triggers run (ringil serve); `ringil trigger` shows how its runs went.", nil
		}, agent.Destructive())
	return strict{inner}
}

// strict refuses a call with a field create_trigger does not take.
type strict struct{ agent.Tool }

func (s strict) Call(ctx context.Context, input json.RawMessage) (agent.ToolOutput, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) == nil {
		for k := range fields {
			if !createKeys[k] {
				return agent.ToolOutput{}, agent.ToolError(agent.ErrToolInput, "create_trigger takes no field "+strconv.Quote(k)+": call it again without it")
			}
		}
	}
	return s.Tool.Call(ctx, input)
}

func (in createIn) trigger() Trigger {
	return Trigger{Name: in.Name, Kind: in.Kind, When: in.When, TimeZone: in.Zone, Tool: in.Tool, Args: in.Args,
		Path: in.Path, Skill: in.Skill, Prompt: in.Prompt, Allow: in.Allow, Timeout: in.Timeout, Continue: in.Keep}
}

// neverAllowed are tools a trigger's runs may never use unasked: making
// triggers (a trigger would widen itself, with no one seeing).
var neverAllowed = map[string]bool{"create_trigger": true}

// refused: what create_trigger never makes.
func refused(t Trigger) error {
	switch t.kind() {
	case Webhook:
		return errors.New(ErrConfig, "webhooks are made by the user (ringil trigger add --kind webhook): their secret is not for the agent")
	case Message:
		return errors.New(ErrConfig, "message triggers are made by the user (ringil trigger add --kind message)")
	}
	for _, name := range t.Allow {
		if neverAllowed[name] {
			return errors.New(ErrConfig, "a trigger's runs may not use "+name+" unasked")
		}
	}
	return nil
}
