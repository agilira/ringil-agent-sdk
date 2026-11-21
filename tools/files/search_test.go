// search_test.go: grep matching rules
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"strings"
	"testing"
)

// 18/11: a small model searched "penale di recesso" in a file with
// "Penale di recesso" and concluded it was not there. Smart case.
func TestGrepSmartCase(t *testing.T) {
	for pattern, want := range map[string]bool{"penale": false, "Penale": true, `\Spenale`: false, `\SPenale`: true, "(?-i)penale": false} {
		if hasUpper(pattern) != want {
			t.Errorf("%q: hasUpper %v", pattern, !want)
		}
	}
}

// 21/11: a glob of a whole home directory walked two million entries for
// 98 s and found nothing. A walk stops at maxWalked and says so; what it
// found stands, and the excluded stay hidden on the way.
func TestWalksStopAtTheirBound(t *testing.T) {
	saved := maxWalked
	maxWalked = 6
	t.Cleanup(func() { maxWalked = saved })
	files := map[string]string{"a/1.txt": "x", "a/2.txt": "x", "b/3.txt": "x", "b/4.txt": "x", "c/5.txt": "x", "c/6.txt": "x", ".ssh/id_rsa": "k"}
	r := open(t, Config{Dirs: []string{tree(t, files)}})
	for name, args := range map[string]map[string]any{
		"glob":           {"pattern": "**/*.txt"},
		"list_directory": {"depth": 0},
		"grep":           {"pattern": "x"},
	} {
		out := mustCall(t, r, name, args)
		if !strings.Contains(out, "stopped after 6 entries") || strings.Contains(out, "id_rsa") || !strings.Contains(out, "1.txt") {
			t.Errorf("%s: %s", name, out)
		}
	}
	maxWalked = 100
	if out := mustCall(t, r, "glob", map[string]any{"pattern": "**/*.txt"}); strings.Contains(out, "stopped") || !strings.Contains(out, "6 files") {
		t.Errorf("under the bound: %s", out)
	}
}
