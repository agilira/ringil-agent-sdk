// security_test.go: the library's names and files at their bounds
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

// No name reaches a file outside the library, and Get never panics on
// what a file holds.
func FuzzLibrary(f *testing.F) {
	f.Add("weekly", `{"name":"weekly","plan":{"steps":[{"name":"a","worker":"w","task":"x"}],"output":"a"}}`)
	f.Add("../secret", `{}`)
	f.Add("a/b", `{"name":"a/b"}`)
	f.Add("C:x", `null`)
	f.Fuzz(func(t *testing.T, name, content string) {
		base := t.TempDir()
		wantNil(t, os.WriteFile(filepath.Join(base, "secret.json"), []byte(`{"name":"secret","plan":{}}`), 0o600))
		lib, err := OpenLibrary(filepath.Join(base, "lib"))
		wantNil(t, err)
		defer func() { _ = lib.Close() }()
		valid := savedName.MatchString(name)
		if valid {
			wantNil(t, os.WriteFile(filepath.Join(base, "lib", name+".json"), []byte(content), 0o600))
		}
		readsOnlyItsOwn(t, lib, name, valid)
		if err := lib.Remove(name); err == nil && !valid {
			t.Fatalf("removed %q", name)
		}
		if _, err := os.Stat(filepath.Join(base, "secret.json")); err != nil {
			t.Fatal("a file outside the library was removed")
		}
		var plan Plan
		if json.Unmarshal([]byte(content), &plan) == nil {
			_ = Saved{Name: "x", Plan: plan}.Check(nil) // never panics
		}
	})
}

// readsOnlyItsOwn: Get reads name only when it is a workflow's name, and
// then as that workflow.
func readsOnlyItsOwn(t *testing.T, lib *Library, name string, valid bool) {
	t.Helper()
	s, err := lib.Get(name)
	if err != nil {
		return
	}
	if !valid || s.Name != name || strings.ContainsAny(name, `/\.:`) {
		t.Fatalf("read %q as %+v", name, s)
	}
}

// A workflow too big for its file is refused before it is written.
func TestLibraryBounds(t *testing.T) {
	lib, err := OpenLibrary(filepath.Join(t.TempDir(), "lib"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lib.Close() }()
	steps := make([]Step, 65)
	for i := range steps {
		steps[i] = Step{Name: "s" + strings.Repeat("a", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Worker: "w", Task: "x"}
	}
	if err := lib.Put(Saved{Name: "big", Plan: Plan{Steps: steps, Output: steps[0].Name}}, nil); !errors.HasCode(err, ErrPlan) {
		t.Fatalf("65 steps saved: %v", err)
	}
	if err := lib.Put(Saved{Name: "big", Brief: strings.Repeat("b", 64<<10+1), Plan: report}, nil); !errors.HasCode(err, ErrPlan) {
		t.Fatalf("a brief over 64 KiB saved: %v", err)
	}
}
