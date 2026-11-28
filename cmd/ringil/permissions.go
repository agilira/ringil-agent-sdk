// permissions.go: ringil.yaml's permissions, on the SDK's Policy
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"slices"
	"sync"

	"github.com/agilira/argus"
	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/serve"
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

// policy is the Policy of one call: the file's rules, those the answers
// added for this asker (a worker's "always" is not the parent's), and in
// a conversation of serve's those its answers kept with it.
func (c *cli) policy(ctx context.Context) *agent.Policy {
	rules, _ := c.conf().Permissions.rules() // checked when the file was read
	who := askerKey(ctx)
	rules = append(rules, c.learned.of(who)...)
	rules = append(rules, c.conversationRules(ctx, who)...)
	mode := c.mode()
	if c.inTrigger && (mode == agent.ModeBypass || mode == agent.ModeAcceptEdits) {
		mode = agent.ModeDefault // --no-approvals or --accept-edits never widens what an automation was allowed
	}
	return &agent.Policy{Mode: mode, Rules: rules, Ask: c.askPerson,
		Remember: func(r agent.Rule, scope agent.Scope) error { return c.remember(ctx, who, r, scope) }}
}

// askerKey names who asks: the parent "", or a worker.
func askerKey(ctx context.Context) string {
	if info, ok := delegate.Current(ctx); ok {
		return info.Worker
	}
	return ""
}

// conversationRules are the rules the answers "yes, in this conversation"
// kept with the conversation of serve's the run continues (its labels:
// they go with it, deleted, exported or imported).
func (c *cli) conversationRules(ctx context.Context, who string) []agent.Rule {
	conv := serve.ConversationOf(ctx)
	if conv == "" || c.labels == nil || c.inTrigger { // an automation does only what it was allowed when it was made
		return nil
	}
	l, err := c.labels.Labels(ctx, conv)
	if err != nil {
		return nil // none read: the person is asked
	}
	var out []agent.Rule
	for _, t := range l.Allowed[who] {
		if r, err := agent.ParseRule(agent.EffectAllow, t); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// maxLearned bounds the askers whose answers are kept in memory.
const maxLearned = 512

// learnedRules are the rules answers added in memory, by asker: the
// terminal's session; in serve, a run's own (forRun gives each its own:
// an "always" then lasts in ringil.yaml, where removing it revokes it).
type learnedRules struct {
	mu    sync.Mutex
	rules map[string][]agent.Rule
}

func (l *learnedRules) of(key string) []agent.Rule {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.rules[key])
}

func (l *learnedRules) add(key string, r agent.Rule) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rules == nil {
		l.rules = map[string][]agent.Rule{}
	}
	if _, known := l.rules[key]; !known && len(l.rules) >= maxLearned {
		for k := range l.rules { // one forgotten asks again, nothing more
			delete(l.rules, k)
			break
		}
	}
	if !slices.ContainsFunc(l.rules[key], func(x agent.Rule) bool { return x.String() == r.String() }) {
		l.rules[key] = append(l.rules[key], r)
	}
}

// remember keeps an answer's rule: "yes, in this conversation" with the
// conversation of serve's; the terminal's session in memory; "always" in
// ringil.yaml's permissions.allow as well.
func (c *cli) remember(ctx context.Context, who string, r agent.Rule, scope agent.Scope) error {
	if conv := serve.ConversationOf(ctx); scope == agent.RememberSession && conv != "" && c.labels != nil {
		if err := c.keepInConversation(ctx, conv, who, r); err != nil {
			c.note("the answer could not be kept with the conversation: it held for this call only (" + describe(err) + ")")
		}
		return nil
	}
	c.learned.add(who, r)
	if scope != agent.RememberProject {
		return nil
	}
	if err := c.keepRule(r); err != nil {
		c.note(describe(err))
	}
	return nil
}

// keepInConversation adds r to the conversation's labels, for who.
func (c *cli) keepInConversation(ctx context.Context, conv, who string, r agent.Rule) error {
	return c.labels.Label(ctx, conv, func(l *agent.Labels) {
		if l.Allowed == nil {
			l.Allowed = map[string][]string{}
		}
		if !slices.Contains(l.Allowed[who], r.String()) && len(l.Allowed[who]) < agent.MaxAllowedRules {
			l.Allowed[who] = append(l.Allowed[who], r.String())
		}
	})
}

// keepRule adds r to ringil.yaml's permissions.allow, through argus's
// writer: the rest of the file stays as it is.
func (c *cli) keepRule(r agent.Rule) error {
	if c.settings == nil || c.settings.path == "" {
		return errors.New(ErrUsage, "no configuration file: the rule holds for this session only")
	}
	if err := c.writeRuleLocked(r); err != nil {
		return err
	}
	return c.settings.settle(settleWait)
}

func (c *cli) writeRuleLocked(r agent.Rule) error {
	defer c.lockKeep()()
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
