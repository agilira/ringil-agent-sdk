// permissions.go: ringil.yaml's permissions, on the SDK's Policy
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"slices"

	"github.com/agilira/argus"
	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/delegate"
)

// permissionsConfig is ringil.yaml's permissions, as coding agents write
// them: a mode, and rules that allow, ask or deny (deny wins, then ask,
// then allow; then the mode). --accept-edits and --no-approvals win over
// the mode for one command.
type permissionsConfig struct {
	// Mode is default, accept-edits, plan or bypass.
	Mode  string   `argus:"mode"`
	Allow []string `argus:"allow"`
	Ask   []string `argus:"ask"`
	Deny  []string `argus:"deny"`
}

var modes = map[string]agent.Mode{"": agent.ModeDefault, "default": agent.ModeDefault, "accept-edits": agent.ModeAcceptEdits,
	"plan": agent.ModePlan, "bypass": agent.ModeBypass}

func (p permissionsConfig) check() error {
	if _, ok := modes[p.Mode]; !ok {
		return errors.New(ErrUsage, "permissions.mode is default, accept-edits, plan or bypass")
	}
	_, err := p.rules()
	return err
}

// rules are the file's rules, read.
func (p permissionsConfig) rules() ([]agent.Rule, error) {
	var rules []agent.Rule
	for effect, texts := range map[agent.Effect][]string{agent.EffectAllow: p.Allow, agent.EffectAsk: p.Ask, agent.EffectDeny: p.Deny} {
		for _, text := range texts {
			r, err := agent.ParseRule(effect, text)
			if err != nil {
				return nil, errors.Wrap(err, ErrUsage, "permissions."+string(effect)+": "+err.Error())
			}
			rules = append(rules, r)
		}
	}
	return rules, nil
}

// mode is the run's: --no-approvals (outside the chats, whose owners
// decide), --accept-edits, else the file's.
func (c *cli) mode() agent.Mode {
	switch {
	case c.unasked():
		return agent.ModeBypass
	case c.acceptEdits:
		return agent.ModeAcceptEdits
	}
	return modes[c.conf().Permissions.Mode]
}

// policy is the Policy of one call: the file's rules and those the answers
// of this session added for this asker (a worker's "always" is not the
// parent's).
func (c *cli) policy(ctx context.Context) *agent.Policy {
	rules, _ := c.conf().Permissions.rules() // checked when the file was read
	key := askerKey(ctx)
	c.learnedMu.Lock()
	rules = append(rules, c.learned[key]...)
	c.learnedMu.Unlock()
	mode := c.mode()
	if c.inTrigger && mode == agent.ModeBypass {
		mode = agent.ModeDefault // --no-approvals never widens what an automation was allowed
	}
	return &agent.Policy{Mode: mode, Rules: rules, Ask: c.askPerson,
		Remember: func(r agent.Rule, scope agent.Scope) error { return c.remember(key, r, scope) }}
}

// askerKey names who asks: the parent, or a worker.
func askerKey(ctx context.Context) string {
	if info, ok := delegate.Current(ctx); ok {
		return info.Worker
	}
	return ""
}

// remember keeps an "always": for the session, and for the project in
// ringil.yaml's permissions.allow as well.
func (c *cli) remember(key string, r agent.Rule, scope agent.Scope) error {
	c.learnedMu.Lock()
	if c.learned == nil {
		c.learned = map[string][]agent.Rule{}
	}
	c.learned[key] = append(c.learned[key], r)
	c.learnedMu.Unlock()
	if scope != agent.RememberProject {
		return nil
	}
	if err := c.keepRule(r); err != nil {
		c.note(describe(err))
	}
	return nil
}

// keepRule adds r to ringil.yaml's permissions.allow, through argus's
// writer: the rest of the file stays as it is.
func (c *cli) keepRule(r agent.Rule) error {
	c.keepMu.Lock()
	defer c.keepMu.Unlock()
	path := c.configName()
	w, err := argus.NewConfigWriter(path, argus.FormatYAML, nil)
	if err != nil {
		return errors.Wrap(err, ErrUsage, path+" cannot be edited: the rule holds for this session only ("+describe(err)+")")
	}
	allow := stringsOf(w.GetValue("permissions.allow"))
	if slices.Contains(allow, r.String()) {
		return nil
	}
	e := &edit{w: w}
	e.set("permissions.allow", append(allow, r.String()))
	if e.err != nil {
		return e.err
	}
	if err := keepBackup(path); err != nil {
		return err
	}
	return w.WriteConfig()
}
