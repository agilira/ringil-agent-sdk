// search_test.go: grep matching rules
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import "testing"

// 18/11: a small model searched "penale di recesso" in a file with
// "Penale di recesso" and concluded it was not there. Smart case.
func TestGrepSmartCase(t *testing.T) {
	for pattern, want := range map[string]bool{"penale": false, "Penale": true, `\Spenale`: false, `\SPenale`: true, "(?-i)penale": false} {
		if hasUpper(pattern) != want {
			t.Errorf("%q: hasUpper %v", pattern, !want)
		}
	}
}
