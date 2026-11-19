// describe.go: an approval, asked as a person would ask it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"encoding/json"
	"strings"
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
		return question{text: sp.t(mAskWrite, f.text("path")), code: f.text("content")}
	}},
	"edit_file": {[]string{"path", "old_string", "new_string", "replace_all"}, func(sp speech, f *callFields) question {
		m := mAskEdit
		if f.flag("replace_all") {
			m = mAskEditAll
		}
		return question{text: sp.t(m, f.text("path")), code: diff(f.text("old_string"), f.text("new_string"))}
	}},
	"write_protected_file": {[]string{"path"}, func(sp speech, f *callFields) question {
		return question{text: sp.t(mAskProtected, f.text("path"))}
	}},
	"bash_": {[]string{"command", "reason", "why", "explanation"}, askCommand},
	"network_domain": {[]string{"domain"}, func(sp speech, f *callFields) question {
		return question{text: sp.t(mAskDomain, f.text("domain"))}
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

// askCommand asks about a command: why it needs a yes (outside the sandbox,
// or what makes it dangerous), and what the agent said of it.
func askCommand(sp speech, f *callFields) question {
	lines := []string{sp.t(mAskCommand)}
	if f.text("reason") == "unsandboxed" {
		lines = append(lines, sp.t(mOutside))
	}
	for _, why := range f.texts("why") {
		lines = append(lines, "• "+why)
	}
	if e := f.text("explanation"); e != "" {
		lines = append(lines, sp.t(mAgentSays, e))
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
