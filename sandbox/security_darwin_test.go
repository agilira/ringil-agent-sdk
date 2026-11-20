// security_darwin_test.go: Seatbelt profile escaping tests
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build darwin

package sandbox

import (
	"strings"
	"testing"
)

func TestSecurityDarwinQuoteAndProfileEscaping(t *testing.T) {
	tricky := `/tmp/dir") (allow default) ("`
	if got, want := quote(tricky), `"/tmp/dir\") (allow default) (\""`; got != want {
		t.Fatalf("quote(%q) = %q, want %q", tricky, got, want)
	}
	prof := buildProfile(Spec{Argv: []string{"/bin/true"}}, []grant{{
		Mount: Mount{Target: "/w", Writable: true},
		real:  tricky,
		dir:   true,
	}}, "/tmp/t", "")
	// The path stays one string: quoted whole, its quote escaped, never
	// closed early (dir") would end the string and open a rule).
	if !strings.Contains(prof, "(subpath "+quote(tricky)+")") || strings.Contains(prof, `dir")`) {
		t.Fatalf("injected Seatbelt rule appeared unescaped in profile:\n%s", prof)
	}
}
