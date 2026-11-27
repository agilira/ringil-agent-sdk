// audit2_test.go: regressions for the audit of saved workflows, 27/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package workflow

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/ringil-agent-sdk/delegate"
)

// A workflow too large to be read back is refused when it is saved,
// and the library keeps listing the others.
func TestLibraryRefusesWhatItCannotRead(t *testing.T) {
	lib, _ := openLib(t)
	crew := &hostCrew{}
	wantNil(t, lib.Put(Saved{Name: "small", Plan: report}, crew.Has))
	big := Saved{Name: "big", Plan: Plan{Steps: []Step{{Name: "a", Worker: "writer", Task: strings.Repeat("x", 1100<<10)}}, Output: "a"}}
	wantCode(t, lib.Put(big, crew.Has), ErrPlan, "1.1 MiB")
	list, err := lib.List()
	wantNil(t, err)
	wantEqual(t, len(list), 1, "listed")
}

// Retry and Resolve of a run or step that is not there say not found.
func TestChangeOfNothingIsNotFound(t *testing.T) {
	j, err := NewFileJournal(t.TempDir())
	wantNil(t, err)
	wantCode(t, Retry(bg, j, "nope", "a"), ErrNotFound, "no run")
	_, err = (&Runner{Crew: &hostCrew{}, Journal: j}).Run(bg, "done", report, "", false)
	wantNil(t, err)
	wantCode(t, Resolve(bg, j, "done", "deck", "x"), ErrNotFound, "a step not left started")
}

func TestLibraryCaseCollisionAndValidation(t *testing.T) {
	lib, dir := openLib(t)
	crew := &hostCrew{}
	wantNil(t, lib.Put(Saved{Name: "Weekly", Plan: report}, crew.Has))
	wantCode(t, lib.Put(Saved{Name: "weekly", Plan: report}, crew.Has), ErrPlan, "case collision")
	wantCode(t, lib.Remove("weekly"), ErrNotFound, "wrong case remove")
	wantNil(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"name":"bad","plan":{}}`), 0o600))
	wantNil(t, os.WriteFile(filepath.Join(dir, "trail.json"), []byte(`{"name":"trail","plan":{"steps":[{"name":"a","worker":"writer","task":"x"}],"output":"a"}}{}`), 0o600))
	_, badErr := lib.Get("bad")
	wantCode(t, badErr, ErrLibrary, "invalid plan")
	_, trailErr := lib.Get("trail")
	wantCode(t, trailErr, ErrLibrary, "trailing json")
	wantNil(t, lib.Remove("bad"))
}

type secretErrCrew struct{}

func (secretErrCrew) Has(w string) bool { return w == "w" }
func (secretErrCrew) Workers() []string { return []string{"w"} }
func (secretErrCrew) RunCounted(context.Context, string, string) (string, delegate.Spend, error) {
	return "", delegate.Spend{}, stderrors.New("/etc/secret-internal-path")
}

func TestRunSavedToolRedactsError(t *testing.T) {
	lib, _ := openLib(t)
	s := Saved{Name: "s", Plan: Plan{Steps: []Step{{Name: "a", Worker: "w", Task: "x"}}, Output: "a"}}
	wantNil(t, lib.Put(s, secretErrCrew{}.Has))
	tool := (&Runner{Crew: secretErrCrew{}, NoJournal: true}).RunSavedTool(lib)
	in, _ := json.Marshal(runSavedIn{Name: "s"})
	out, err := tool.Call(bg, in)
	wantCode(t, err, ErrStep, "step error")
	if out.Content != "" || strings.Contains(err.Error(), "secret-internal-path") {
		t.Fatalf("leaked internal error: err=%v out=%+v", err, out)
	}
}
