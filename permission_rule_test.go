// permission_rule_test.go: the rules an answer learns
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import "testing"

// A pattern in a path learns no rule: **/x.txt would allow every file.
func TestRuleForNoPatternInAPath(t *testing.T) {
	spec := ToolSpec{Name: "write_file", InputSchema: []byte(`{"properties":{"path":{"type":"string"}}}`), Annotations: Annotations{Edits: true}}
	if _, ok := RuleFor(ToolCall{Name: "write_file", Input: []byte(`{"path":"notes/a.txt"}`)}, spec); !ok {
		t.Fatal("a plain path learns no rule")
	}
	for _, p := range []string{"**/x.txt", "*/x.txt", "[a-z]*/x", "a?/x"} {
		if r, ok := RuleFor(ToolCall{Name: "write_file", Input: []byte(`{"path":"` + p + `"}`)}, spec); ok {
			t.Errorf("%s learned %s", p, r)
		}
	}
}
