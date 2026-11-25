// security_test.go: fuzz and boundaries of Within
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package dirs

import (
	"path/filepath"
	"strings"
	"testing"
)

// Within never says a path is inside a directory it escapes with ..,
// whatever the spelling.
func FuzzWithin(f *testing.F) {
	for _, s := range []string{"/a/b", "/a/../b", "/a/./b/../../c", "a/b", "/a//b", "/a/b/..", "/a/.."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if strings.ContainsRune(p, 0) {
			return
		}
		dir := filepath.FromSlash("/base/dir")
		if !Within(p, dir) {
			return
		}
		abs, err := filepath.Abs(p)
		abs, dir = fold(abs), fold(dir)
		if err != nil || !(abs == dir || strings.HasPrefix(abs, dir+string(filepath.Separator))) {
			t.Fatalf("%q said inside %s (%s)", p, dir, abs)
		}
	})
}
