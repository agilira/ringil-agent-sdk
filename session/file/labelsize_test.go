// labelsize_test.go: labels as large as Check lets them are read back whole (audit 27/11)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package file

import (
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
)

func TestLargeLabelsReadBackWhole(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ls := st.(agent.LabelStore)
	ctx := t.Context()
	if err := ls.Label(ctx, "c1", func(l *agent.Labels) { l.Title = "My chat"; l.Project = "proj1"; l.Pinned = true }); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		r := "bash(git log --format=" + strings.Repeat("x", 40) + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "*)"
		if err := ls.Label(ctx, "c1", func(l *agent.Labels) {
			if l.Allowed == nil {
				l.Allowed = map[string][]string{}
			}
			l.Allowed[""] = append(l.Allowed[""], r)
		}); err != nil {
			t.Fatal(i, err)
		}
		l, _ := ls.Labels(ctx, "c1")
		if l.Title == "" {
			t.Fatalf("after %d rules the labels read back zero: %+v", i+1, l)
		}
	}
}
