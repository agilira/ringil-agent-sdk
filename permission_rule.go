// permission_rule.go: the rules of a Policy, and the calls they match
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"encoding/json"
	"path"
	"slices"
	"strings"

	"github.com/agilira/go-errors"
)

// Rule is one line of a policy, written as coding agents write them:
//
//	bash                   every bash call
//	bash(git status:*)     a command starting with these words
//	bash(npm test)         this command exactly
//	write_file(src/**)     a path matching the pattern (** spans folders)
//	fetch(domain:go.dev)   a URL of this host or one under it
//	mcp_github_*           tools whose name matches
//
// A pattern is matched against the argument the tool's schema declares,
// the first of "command" (words), "path" (a glob) and "url" (a domain, the
// "domain:" prefix optional); a tool without one matches no pattern. An
// argument the model adds outside the schema is never read: it is dropped
// before the tool runs. Allow rules are matched strictly (one plain command, the path
// as given), deny and ask rules loosely, failing closed: a command's every
// part and word, a path's every tail, case and all, a host that cannot be
// read.
type Rule struct {
	Effect  Effect
	Tool    string // a name, or a pattern of names
	Pattern string // "" for every call of the tool
}

// ErrRule: a rule is not one a policy reads.
const ErrRule errors.ErrorCode = "PERMISSION_RULE"

// ParseRule reads a rule in the form above.
func ParseRule(effect Effect, text string) (Rule, error) {
	if effect != EffectAllow && effect != EffectAsk && effect != EffectDeny {
		return Rule{}, errors.New(ErrRule, "a rule's effect is allow, ask or deny")
	}
	tool, pattern, err := splitRule(strings.TrimSpace(text))
	if err != nil {
		return Rule{}, err
	}
	if _, err := path.Match(tool, ""); err != nil || tool == "" || strings.ContainsAny(tool, " /") {
		return Rule{}, errors.New(ErrRule, "a rule names a tool, or a pattern of tool names: "+text)
	}
	return Rule{Effect: effect, Tool: tool, Pattern: pattern}, nil
}

// splitRule is tool and pattern of "tool(pattern)", or tool alone.
func splitRule(text string) (tool, pattern string, err error) {
	tool, pattern, hasPattern := strings.Cut(text, "(")
	if !hasPattern {
		return tool, "", nil
	}
	pattern, ok := strings.CutSuffix(pattern, ")")
	if !ok || strings.TrimSpace(pattern) == "" {
		return "", "", errors.New(ErrRule, "a rule is tool or tool(pattern): "+text)
	}
	return tool, strings.TrimSpace(pattern), nil
}

// String is the rule as ParseRule reads it.
func (r Rule) String() string {
	if r.Pattern == "" {
		return r.Tool
	}
	return r.Tool + "(" + r.Pattern + ")"
}

// matchAny: a rule of effect matches the call.
func matchAny(rules []Rule, effect Effect, call ToolCall, spec ToolSpec) bool {
	for _, r := range rules {
		if r.Effect == effect && r.matches(call, spec) {
			return true
		}
	}
	return false
}

// matches: the rule's tool, and its pattern on the argument the tool's
// schema declares for that kind of pattern.
func (r Rule) matches(call ToolCall, spec ToolSpec) bool {
	if ok, _ := path.Match(r.Tool, spec.Name); !ok {
		return false
	}
	if r.Pattern == "" {
		return true
	}
	key := subjectKey(spec)
	if key == "" {
		return false
	}
	subject, ok := argument(call, key)
	if !ok || subject == "" {
		return r.Effect != EffectAllow // what cannot be read is refused or asked, never allowed
	}
	switch key {
	case "url":
		return r.hostMatches(strings.TrimPrefix(r.Pattern, "domain:"), subject)
	case "command":
		return r.commandMatches(subject)
	}
	return r.pathMatches(subject)
}

// unreadableUnder: a deny or ask rule is about this command tool, and the
// command's words cannot be read.
func unreadableUnder(rules []Rule, call ToolCall, spec ToolSpec) bool {
	if subjectKey(spec) != "command" {
		return false
	}
	command, ok := argument(call, "command")
	if !ok || !unreadable(command) {
		return false
	}
	for _, r := range rules {
		if matched, _ := path.Match(r.Tool, spec.Name); matched && r.Effect != EffectAllow && r.Pattern != "" {
			return true
		}
	}
	return false
}

// subjectKey is the argument a tool's rules are about: the first of
// "command", "path", "url" its schema declares; "" for none.
func subjectKey(spec ToolSpec) string {
	for _, key := range []string{"command", "path", "url"} {
		if declares(spec, key) {
			return key
		}
	}
	return ""
}

// declares: the tool's schema has the argument key (an argument the model
// adds is dropped before the tool runs: matching it would judge one value
// and run another).
func declares(spec ToolSpec, key string) bool {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(spec.InputSchema, &schema) != nil {
		return false
	}
	_, ok := schema.Properties[key]
	return ok
}

// argument is the string the call gives key, if it gives one.
func argument(call ToolCall, key string) (string, bool) {
	var args map[string]json.RawMessage
	if json.Unmarshal(call.Input, &args) != nil {
		return "", false
	}
	raw, ok := args[key]
	if !ok {
		return "", true
	}
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	return v, true
}

// RuleFor is the allow rule an "always" answer keeps for call, and false
// when there is none narrow enough: then the answer holds for this call
// only. A command keeps its first two words, when it is one plain command
// whose program runs nothing else; a path its folder, not the current
// one, the root or one above; a URL its host; a tool without such an
// argument its name.
func RuleFor(call ToolCall, spec ToolSpec) (Rule, bool) {
	rule := Rule{Effect: EffectAllow, Tool: spec.Name}
	key := subjectKey(spec)
	if key == "" {
		return rule, true
	}
	subject, ok := argument(call, key)
	if !ok || subject == "" {
		return Rule{}, false
	}
	rule.Pattern, ok = learnedPattern(key, subject)
	return rule, ok
}

func learnedPattern(key, subject string) (string, bool) {
	switch key {
	case "command":
		return learnedCommand(subject)
	case "url":
		host, ok := urlHost(subject)
		return "domain:" + host, ok && host != ""
	}
	return learnedFolder(subject)
}

func learnedCommand(command string) (string, bool) {
	words := strings.Fields(command)
	switch {
	case !plainCommand(command) || len(words) == 0:
		return "", false
	case slices.ContainsFunc(words, func(w string) bool { return riskyVerbs[w] }):
		return commandWords(command), true // this command exactly (git branch -D, docker container rm)
	case len(words) > 1 && strings.HasPrefix(words[1], "-"):
		return "", false // git -C, git -c: options that change what runs
	}
	return strings.Join(words[:min(2, len(words))], " ") + ":*", true
}

// riskyVerbs are first or second words whose other words decide the harm:
// an "always" keeps the command exactly, never its kind.
var riskyVerbs = map[string]bool{
	"rm": true, "rmdir": true, "delete": true, "del": true, "remove": true, "run": true, "exec": true, "install": true,
	"uninstall": true, "config": true, "push": true, "apply": true, "destroy": true, "drop": true, "kill": true,
	"chmod": true, "chown": true, "mv": true, "dd": true, "mkfs": true, "truncate": true, "reset": true, "clean": true,
	"prune": true, "curl": true, "wget": true, "scp": true, "rsync": true, "-D": true, "-d": true, "--delete": true,
	"-f": true, "--force": true, "-rf": true, "-fr": true, "-r": true, "-R": true,
}

func learnedFolder(p string) (string, bool) {
	if strings.HasPrefix(p, "~") || strings.ContainsAny(p, "*?[") { // a pattern in the path: the rule would hold far more than the call
		return "", false
	}
	dir := path.Dir(cleanPath(p))
	if dir == "." || dir == "/" || dir == ".." || strings.HasPrefix(dir, "../") || tooWide(dir) {
		return "", false
	}
	return path.Join(dir, "**"), true
}

// tooWide: an absolute folder that holds far more than a project: a top
// folder, a drive's root, a home (/home/u, /Users/u, C:/Users/u, /root).
func tooWide(dir string) bool {
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	if len(parts) > 0 && strings.HasSuffix(parts[0], ":") {
		parts = parts[1:] // C:
		dir = "/" + strings.Join(parts, "/")
	}
	if !path.IsAbs(dir) {
		return false
	}
	switch {
	case len(parts) < 2:
		return true
	case parts[0] == "home" || strings.EqualFold(parts[0], "Users"):
		return len(parts) < 3
	}
	return false
}
