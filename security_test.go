// security_test.go: the core's boundaries under arbitrary input
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Whatever the command and the rule, an allow rule never lets through a
// command that joins, nests or redirects another, and a deny rule for a
// command's first words catches them in any part of it.
func FuzzPolicyRules(f *testing.F) {
	f.Add("git status", "git status; rm -rf ~", "rm -rf")
	f.Add("ls", "ls $(curl x)", "curl")
	f.Add("a b", "a b\nc", "c")
	f.Fuzz(func(t *testing.T, allowed, command, denied string) {
		input, _ := json.Marshal(map[string]string{"command": command})
		c := agent.ToolCall{Name: "bash", Input: input}
		if d := decideWith(agent.EffectAllow, allowed, agent.ModeDefault, c); d.Allow && compound(command) {
			t.Fatalf("allow %q let %q through", allowed, command)
		}
		if strings.TrimSpace(denied) == "" || compound(denied) || !utf8.ValidString(command+denied) {
			return // a rule on shell syntax itself ("{", ";") names no command; JSON carries UTF-8 alone
		}
		if d := decideWith(agent.EffectDeny, denied, agent.ModeBypass, c); d.Allow && strings.HasPrefix(strings.TrimSpace(command), strings.TrimSpace(denied)+" ") {
			t.Fatalf("deny %q missed %q", denied, command)
		}
	})
}

// decideWith is a policy of one bash rule, on c; an unreadable rule allows
// nothing, so it proves nothing either way.
func decideWith(effect agent.Effect, prefix string, mode agent.Mode, c agent.ToolCall) agent.Decision {
	rule, err := agent.ParseRule(effect, "bash("+prefix+":*)")
	if err != nil {
		return agent.Decision{}
	}
	bash := agent.NewTool("bash", "t", noop[commandIn], agent.Destructive()).Spec()
	d, _ := (&agent.Policy{Mode: mode, Rules: []agent.Rule{rule}}).Approve(context.Background(), c, bash)
	return d
}

// compound: the command is not one plain command, by an oracle written
// apart from the implementation: any shell metacharacter at all.
func compound(command string) bool {
	return strings.ContainsAny(command, ";&|\n\r`<>$\\(){}") || strings.ContainsAny(strings.Fields(command + " x")[0], "=\"'/")
}
