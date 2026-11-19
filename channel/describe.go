// describe.go: an approval, asked as a person would ask it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// question is an approval as the chat reads it: a sentence, and what it
// is about (a file's content, a command) shown as it is.
type question struct {
	text, code string
}

// describe asks about a tool call in words, for the tools whose input it
// knows: writing and changing files, commands, network domains. Any
// other tool, or a known one with a field it does not know, is asked
// about by name, with every field shown: nothing the call does is left
// out of what is read.
func describe(sp speech, tool string, raw json.RawMessage) question {
	if tool == "create_trigger" {
		return askTrigger(sp, raw)
	}
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) == nil {
		if q, ok := known(sp, tool, in); ok {
			return q
		}
	}
	q := question{text: sp.t(mAskTool, visible(tool))}
	if string(raw) != "{}" && len(raw) > 0 {
		q.code = showInput(raw)
	}
	return q
}

// asking is how a known tool's calls are asked about: the fields they
// may have, and the question.
type asking struct {
	fields []string
	ask    func(sp speech, f *callFields) question
}

// knownTools are the tools asked about in words; bash_ is every reason a
// command is asked about.
var knownTools = map[string]asking{
	"write_file": {[]string{"path", "content"}, func(sp speech, f *callFields) question {
		return question{text: sp.t(mAskWrite, f.path("path")), code: f.text("content")}
	}},
	"edit_file": {[]string{"path", "old_string", "new_string", "replace_all"}, func(sp speech, f *callFields) question {
		m := mAskEdit
		if f.flag("replace_all") {
			m = mAskEditAll
		}
		return question{text: sp.t(m, f.path("path")), code: diff(f.text("old_string"), f.text("new_string"))}
	}},
	"write_protected_file": {[]string{"path"}, func(sp speech, f *callFields) question {
		return question{text: sp.t(mAskProtected, f.path("path"))}
	}},
	"bash_": {[]string{"command", "reason", "why", "explanation"}, askCommand},
	"create_skill": {[]string{"name", "description", "instructions", "replace"}, func(sp speech, f *callFields) question {
		m := mAskSkill
		if f.flag("replace") {
			m = mAskSkillReplace
		}
		return question{text: sp.t(m, inline(f.text("name")), quote(f.text("description"))), code: f.text("instructions")}
	}},
	"network_domain": {[]string{"domain"}, func(sp speech, f *callFields) question {
		return question{text: sp.t(mAskDomain, inline(f.text("domain")))}
	}},
}

// known describes the calls of the tools it knows; false when the call
// has fields of other kinds or names.
func known(sp speech, tool string, in map[string]json.RawMessage) (question, bool) {
	name := tool
	if strings.HasPrefix(tool, "bash_") {
		name = "bash_"
	}
	k, ok := knownTools[name]
	f := fieldsOf(in)
	if !ok || !f.only(k.fields...) {
		return question{}, false
	}
	q := k.ask(sp, f)
	return q, f.ok
}

// askTrigger asks about a trigger the agent would make: its name and
// what its runs may do without asking in the sentence, every field below,
// for it is asked once and then runs on its own.
func askTrigger(sp speech, raw json.RawMessage) question {
	var fields map[string]json.RawMessage // exact keys: "NAME" is not "name"
	_ = json.Unmarshal(raw, &fields)
	f := fieldsOf(fields)
	allowed := sp.t(mNothing)
	if list := f.texts("allow"); len(list) > 0 {
		names := make([]string, len(list))
		for i, a := range list {
			names[i] = inline(a)
		}
		allowed = strings.Join(names, ", ")
	}
	return question{text: sp.t(mAskTrigger, inline(f.text("name")), allowed), code: showInput(raw)}
}

// askCommand asks about a command: why it needs a yes (outside the sandbox,
// or what makes it dangerous), and what the agent said of it.
func askCommand(sp speech, f *callFields) question {
	lines := []string{sp.t(mAskCommand)}
	if f.text("reason") == "unsandboxed" {
		lines = append(lines, sp.t(mOutside))
	}
	for _, why := range f.texts("why") {
		lines = append(lines, "• "+inline(why))
	}
	if e := f.text("explanation"); e != "" {
		lines = append(lines, sp.t(mAgentSays, quote(e)))
	}
	return question{text: strings.Join(lines, "\n"), code: f.text("command")}
}

// diff shows a change: the lines that go with "- ", those that come
// with "+ ".
func diff(old, new string) string {
	var lines []string
	for _, l := range strings.Split(old, "\n") {
		lines = append(lines, "- "+l)
	}
	for _, l := range strings.Split(new, "\n") {
		lines = append(lines, "+ "+l)
	}
	return strings.Join(lines, "\n")
}

// callFields reads a call's fields; ok turns false at the first one not of
// the kind asked for.
type callFields struct {
	in map[string]json.RawMessage
	ok bool
}

func fieldsOf(in map[string]json.RawMessage) *callFields {
	return &callFields{in: in, ok: true}
}

// only: the call has no field but these.
func (f *callFields) only(names ...string) bool {
	for name := range f.in {
		found := false
		for _, n := range names {
			found = found || n == name
		}
		if !found {
			return false
		}
	}
	return true
}

// text is a text field, made visible; "" when absent.
func (f *callFields) text(name string) string {
	raw, has := f.in[name]
	if !has {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		f.ok = false
	}
	return visible(s)
}

// path is a path field as the tools read it (cleaned: "a\n\n/../b" is
// b), on one line.
func (f *callFields) path(name string) string {
	p := f.text(name)
	if p == "" {
		return inline(p)
	}
	return inline(filepath.Clean(p))
}

// inline is a value inside a sentence: as it is when it reads plainly on
// one line, else quoted, its line breaks and odd blanks escaped, so that
// it cannot make lines or gaps of its own.
func inline(s string) string {
	plain := s != "" && strings.TrimSpace(s) == s && !strings.Contains(s, "  ") && !strings.ContainsAny(s, "\"")
	for _, r := range s {
		if unicode.IsSpace(r) && r != ' ' {
			plain = false
		}
	}
	if plain {
		return s
	}
	return quote(s)
}

// quote puts s in double quotes, escaping quotes, backslashes, every
// blank but the space and whatever does not print; letters of any
// language stay as they are.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteString("\\" + string(r))
		case r == ' ' || (unicode.IsGraphic(r) && !unicode.IsSpace(r)):
			b.WriteRune(r)
		default:
			b.WriteString(strings.Trim(strconv.QuoteRuneToASCII(r), "'"))
		}
	}
	b.WriteByte('"')
	return b.String()
}

// texts is a list of texts, made visible.
func (f *callFields) texts(name string) []string {
	raw, has := f.in[name]
	if !has || string(raw) == "null" {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		f.ok = false
	}
	for i := range list {
		list[i] = visible(list[i])
	}
	return list
}

// flag is a true/false field; false when absent.
func (f *callFields) flag(name string) bool {
	raw, has := f.in[name]
	if !has {
		return false
	}
	var v bool
	if json.Unmarshal(raw, &v) != nil {
		f.ok = false
	}
	return v
}
