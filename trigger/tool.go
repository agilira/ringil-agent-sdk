// tool.go: create_trigger, triggers the agent makes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

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
	From    string          `json:"from,omitempty" desc:"When it starts working: 2026-12-01 or 2026-12-01 09:00 (in time_zone); default now"`
	Until   string          `json:"until,omitempty" desc:"When it stops working and is removed: 2026-12-31 (the whole day) or 2026-12-31 18:00; default never"`
	Timeout string          `json:"timeout,omitempty" desc:"Stop a run that takes longer, from 1m to 2h; default 10m"`
	Keep    bool            `json:"continue,omitempty" desc:"Carry on one conversation across runs (each run remembers the last)"`
}

// createKeys are the only fields create_trigger takes, exactly: what the
// user is shown is what is made, so no field is dropped or read another
// way (a "Name" beside a "name").
var createKeys = map[string]bool{"name": true, "kind": true, "when": true, "time_zone": true, "tool": true, "args": true,
	"path": true, "skill": true, "prompt": true, "allow": true, "timeout": true, "continue": true, "from": true, "until": true}

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
			if t.Ended(time.Now()) {
				return "", errors.New(ErrConfig, "its period is over already: until is in the past")
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
		Path: in.Path, Skill: in.Skill, Prompt: in.Prompt, Allow: in.Allow, Timeout: in.Timeout, Continue: in.Keep,
		From: in.From, Until: in.Until}
}

// neverAllowed are tools a trigger's runs may never use unasked: making
// triggers (a trigger would widen itself, with no one seeing).
var neverAllowed = map[string]bool{"create_trigger": true, "delete_trigger": true}

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

// Describe says in a line what sets a trigger off and when it works.
func Describe(t Trigger) string {
	what := setsOff(t)
	if t.TimeZone != "" {
		what += " " + t.TimeZone
	}
	for _, part := range [][2]string{{", from ", t.From}, {", until ", t.Until}} {
		if part[1] != "" {
			what += part[0] + part[1]
		}
	}
	if t.Paused {
		what += ", paused"
	}
	return what
}

// setsOff says what sets a trigger off.
func setsOff(t Trigger) string {
	switch t.kind() {
	case Tool:
		return "when the answer of " + t.Tool + " changes (" + cmpOr(t.When, defaultTool) + ")"
	case File:
		return "when files change in " + t.Path + " (" + cmpOr(t.When, defaultFile) + ")"
	case Message:
		if t.Match != "" {
			return "on a message in " + t.Chat + " with \"" + t.Match + "\""
		}
		return "on a message in " + t.Chat
	case Webhook:
		return "on a request to its webhook"
	}
	return "on " + t.When
}

// ListTool is list_triggers: the triggers there are, for the agent to
// answer "what automations do you have?" and to name one to delete.
func ListTool(open func() (*Store, error)) agent.Tool {
	return agent.NewTool("list_triggers",
		"List the automations (triggers) that run by themselves: what sets each off, what it does, how its last run went. "+
			"Use it when the user asks what automations there are, or before deleting one.",
		func(_ context.Context, _ struct{}) (string, error) {
			store, err := open()
			if err != nil {
				return "", err
			}
			defer func() { _ = store.Close() }()
			list, err := store.Triggers()
			if err != nil {
				return "", err
			}
			status, _ := store.Status()
			if len(list) == 0 {
				return "No automations.", nil
			}
			var b strings.Builder
			for _, t := range list {
				b.WriteString(listLine(t, status[t.Name]))
			}
			return b.String(), nil
		}, agent.ReadOnly())
}

func listLine(t Trigger, st Status) string {
	line := "- " + t.Name + ": " + Describe(t)
	if t.Skill != "" {
		line += "; skill " + t.Skill
	}
	if p := strings.TrimSpace(t.Prompt); p != "" {
		line += "; does: " + clipTo(p, 200)
	}
	if len(t.Allow) > 0 {
		line += "; may use unasked: " + strings.Join(t.Allow, ", ")
	}
	switch {
	case st.LastError != "":
		line += "; last run failed: " + clipTo(st.LastError, 200)
	case !st.LastFire.IsZero():
		line += "; last run " + st.LastFire.Format("2006-01-02 15:04")
	}
	return line + "\n"
}

type deleteIn struct {
	Name string `json:"name" desc:"The trigger's name, as list_triggers gives it"`
}

// DeleteTool is delete_trigger: the agent removes a trigger the user no
// longer wants. It is marked destructive: the host asks, naming it.
// deleted, when not nil, hears of each one removed.
func DeleteTool(open func() (*Store, error), deleted func(name string)) agent.Tool {
	return agent.NewTool("delete_trigger",
		"Delete an automation (trigger) the user no longer wants: it will not set off again (a run going finishes). Find its name with list_triggers.",
		func(_ context.Context, in deleteIn) (string, error) {
			store, err := open()
			if err != nil {
				return "", err
			}
			err = store.Remove(in.Name)
			_ = store.Close()
			if err != nil {
				return "", err
			}
			if deleted != nil {
				deleted(in.Name)
			}
			return "Trigger " + in.Name + " deleted.", nil
		}, agent.Destructive())
}
