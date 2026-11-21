// compact_test.go: folding hidden paths into their directories
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/agilira/ringil-agent-sdk/sandbox"
)

// Whatever is folded, everything hidden before stays hidden: each path is
// hidden or under a hidden directory.
func covered(t *testing.T, before, after []string) {
	t.Helper()
	for _, h := range before {
		ok := false
		for _, a := range after {
			if h == a || strings.HasPrefix(h, a+"/") {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("%s is no longer hidden: %v", h, after)
		}
	}
}

func TestCompactFoldsTheDeepestFirst(t *testing.T) {
	hide := []string{".ssh", "go/pkg/mod/x@v1/testdata/a.pem", "go/pkg/mod/x@v1/testdata/b.pem", "go/pkg/mod/x@v1/testdata/c.key",
		"proj/.env", "sdk/a/tls/1.pem", "sdk/a/tls/2.pem"}
	got := compact(hide, 4, func(string) bool { return false })
	covered(t, hide, got)
	if len(got) > 4 || !slices.Contains(got, "go/pkg/mod/x@v1/testdata") || !slices.Contains(got, "sdk/a/tls") || !slices.Contains(got, ".ssh") || !slices.Contains(got, "proj/.env") {
		t.Fatalf("%v", got)
	}
	if got := compact(hide, 100, func(string) bool { return false }); !slices.Equal(got, hide) {
		t.Fatalf("within the budget it changed: %v", got)
	}
}

// A directory holding another mount (the working directory inside the
// home) is never hidden: that mount would vanish under it.
func TestCompactNeverHidesAMount(t *testing.T) {
	var hide []string
	for i := range 2000 {
		hide = append(hide, fmt.Sprintf("work/proj/sub%d/k.pem", i%2), fmt.Sprintf("go/m%d/t.pem", i))
	}
	mounts := []sandbox.Mount{{Source: "/home/u", Hide: hide}, {Source: "/home/u/work/proj", Writable: true}}
	fit(mounts)
	got := mounts[0].Hide
	covered(t, hide, got)
	if hiddenCount(mounts) > sandbox.MaxHidden {
		t.Fatalf("%d still hidden", hiddenCount(mounts))
	}
	for _, h := range got {
		if h == "work" || h == "work/proj" {
			t.Fatalf("hid the mount's directory: %s", h)
		}
	}
}
