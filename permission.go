// permission.go: which tool calls run, which ask, which are refused
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"sync"

	"github.com/agilira/go-errors"
)

// Mode is how much a Policy lets run without asking, as coding agents
// name them.
type Mode string

const (
	// ModeDefault: reading runs; changing files, risky and outward calls
	// ask.
	ModeDefault Mode = "default"
	// ModeAcceptEdits: as default, and changes to files run without asking.
	ModeAcceptEdits Mode = "accept-edits"
	// ModePlan: reading and the agent's own bookkeeping only; anything
	// that changes files, runs commands or reaches out is refused.
	ModePlan Mode = "plan"
	// ModeBypass: everything runs but what a deny rule refuses. Never a
	// default: the host chooses it.
	ModeBypass Mode = "bypass"
)

// Effect is what a rule does to the calls it matches.
type Effect string

const (
	// EffectAllow runs the call without asking.
	EffectAllow Effect = "allow"
	// EffectAsk asks, whatever the mode.
	EffectAsk Effect = "ask"
	// EffectDeny refuses the call, whatever the mode.
	EffectDeny Effect = "deny"
)

// Scope is how long an answer's "always" lasts.
type Scope int

const (
	// RememberNone: this call only.
	RememberNone Scope = iota
	// RememberSession: this Policy's life; added to its rules.
	RememberSession
	// RememberProject: kept by the host as well (Policy.Remember), so the
	// next session starts with it.
	RememberProject
)

// Answer is a person's answer to a call the policy asked about.
type Answer struct {
	Allow bool
	// Remember, with Allow, keeps an allow rule for calls like this one
	// (the tool, and for bash the command's first words; see RuleFor).
	Remember Scope
	// Message, without Allow, is what the model is told.
	Message string
}

// Policy is the standard Config.Approve: a mode and rules, as the coding
// agents people know apply them (deny wins, plan mode refuses changes
// whatever allows them, then ask, allow, and the mode). With no rule for a
// call, reading (ReadOnly) and the agent's own bookkeeping (Internal) run,
// a tool that confines its own risky calls (SelfConfirms: bash and skill
// scripts in the sandbox) runs, and everything else asks: changing files,
// destructive and outward calls, and a tool marked nothing. A call that must ask with no
// one to ask (Ask nil: a trigger, a server without a person) is refused:
// the policy fails closed.
//
// Its zero value is the default mode with no rules and no one to ask: the
// agent's default when Config.Approve is nil. A Policy is safe for
// concurrent use once made: its fields are not changed after.
type Policy struct {
	Mode  Mode
	Rules []Rule
	// Ask asks a person; nil refuses what would ask.
	Ask func(ctx context.Context, call ToolCall, spec ToolSpec) (Answer, error)
	// Remember hands the host a rule an answer asked to keep, with how
	// long: for RememberProject the host writes it where its next session
	// reads it. The Policy keeps it for its own life either way; a host
	// that makes a Policy for each call keeps it itself.
	Remember func(rule Rule, scope Scope) error

	mu      sync.Mutex
	learned []Rule // rules the answers added in this session
}

// errNoOneToAsk refuses a call that would ask, with no one to ask.
var errNoOneToAsk = errors.New(ErrToolDenied, "the call needs a person's approval and there is no one to ask")

// Approve implements Config.Approve.
func (p *Policy) Approve(ctx context.Context, call ToolCall, spec ToolSpec) (Decision, error) {
	switch p.Effect(call, spec) {
	case EffectAllow:
		return Allow(), nil
	case EffectDeny:
		return Deny(p.refusal(call, spec)), nil
	}
	if p.Ask == nil {
		return Deny("This call needs the user's approval, and no one can be asked here: it was not run. Do not try it again in this run."), errNoOneToAsk
	}
	answer, err := p.Ask(ctx, call, spec)
	if err != nil || !answer.Allow {
		return Deny(answer.Message), err
	}
	return Allow(), p.learn(call, spec, answer.Remember)
}

// Effect is what the policy does with call, without asking anyone: a deny
// rule refuses; plan mode refuses what is neither reading nor the agent's
// own bookkeeping, whatever allow rule there is; a command a deny or ask
// rule cannot read (an expansion hides its words) asks; then ask rules,
// allow rules, and the mode.
func (p *Policy) Effect(call ToolCall, spec ToolSpec) Effect {
	rules := p.rules()
	a := spec.Annotations
	switch {
	case matchAny(rules, EffectDeny, call, spec):
		return EffectDeny
	case p.Mode == ModePlan && !a.ReadOnly && !a.Internal:
		return EffectDeny
	case unreadableUnder(rules, call, spec):
		return EffectAsk // a deny rule cannot read it: a person decides
	case matchAny(rules, EffectAsk, call, spec):
		return EffectAsk
	case matchAny(rules, EffectAllow, call, spec):
		return EffectAllow
	}
	return p.byMode(a)
}

// AskedByRule: an ask rule matches call, or a deny or ask rule cannot read
// its command. A host that runs calls approved beforehand (an automation)
// refuses these, as no one is there; the mode's own asking it may override.
func (p *Policy) AskedByRule(call ToolCall, spec ToolSpec) bool {
	rules := p.rules()
	return unreadableUnder(rules, call, spec) || matchAny(rules, EffectAsk, call, spec)
}

// byMode is the effect of a call no rule matched: reading, bookkeeping and
// a tool that confines its own risky calls run; a plain edit runs under
// accept-edits; anything else asks, a tool marked nothing too.
func (p *Policy) byMode(a Annotations) Effect {
	switch {
	case p.Mode == ModeBypass, a.ReadOnly, a.Internal, a.SelfConfirms:
		return EffectAllow
	case p.Mode == ModeAcceptEdits && a.Edits && !a.Destructive && !a.OpenWorld:
		return EffectAllow
	}
	return EffectAsk
}

// refusal is what the model is told of a call the policy refused.
func (p *Policy) refusal(call ToolCall, spec ToolSpec) string {
	if p.Mode == ModePlan && !spec.Annotations.ReadOnly && !spec.Annotations.Internal && !matchAny(p.rules(), EffectDeny, call, spec) {
		return "Plan mode: only reading is allowed. Present the plan; the user switches mode to carry it out."
	}
	return "A rule of the user refuses this call: do not try it another way."
}

// rules are the host's rules and those learned in this session.
func (p *Policy) rules() []Rule {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append(append([]Rule(nil), p.Rules...), p.learned...)
}

// learn keeps the allow rule an answer asked for, when there is a narrow
// one to keep (RuleFor); else the answer holds for this call only.
func (p *Policy) learn(call ToolCall, spec ToolSpec, scope Scope) error {
	if scope == RememberNone {
		return nil
	}
	rule, ok := RuleFor(call, spec)
	if !ok {
		return nil
	}
	p.mu.Lock()
	p.learned = append(p.learned, rule)
	p.mu.Unlock()
	if p.Remember != nil {
		return p.Remember(rule, scope)
	}
	return nil
}

// defaultPolicy is the agent's Approve when Config.Approve is nil: the
// default mode, no rules, no one to ask. Reading and the agent's own
// bookkeeping run; what would ask is refused. Unexported: no package can
// change every agent's policy.
var defaultPolicy = &Policy{}

// approver is the host's Approve, or the default policy.
func (c *Config) approver() func(ctx context.Context, call ToolCall, spec ToolSpec) (Decision, error) {
	if c.Approve != nil {
		return c.Approve
	}
	return defaultPolicy.Approve
}
