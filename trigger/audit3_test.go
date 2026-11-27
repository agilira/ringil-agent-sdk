// audit3_test.go: regressions for the audit of triggers, 27/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A trigger's runs may never save or run workflows unasked: it would
// rewrite the workflow it runs, or start others.
func TestTriggersNeverAllowWorkflows(t *testing.T) {
	for _, tool := range []string{"save_workflow", "run_saved_workflow"} {
		tr := Trigger{Name: "a", When: "@daily", Workflow: "weekly", Allow: []string{tool}}
		if err := refused(tr); err == nil {
			t.Errorf("%s allowed", tool)
		}
	}
}

// A File trigger run by hand takes note of what its run wrote, as one set
// off by a change does: it does not set itself off.
func TestRunNowFileRebases(t *testing.T) {
	dir := t.TempDir()
	tr := Trigger{Name: "inbox", Kind: File, Path: dir, When: "@every 1m", Prompt: "x"}
	store, clock, rec := harness(t, tr)
	rec.write = filepath.Join(dir, "report.txt") // the run writes in the folder it watches
	s := launch(t, store, clock, rec)
	clock.advance(time.Minute) // the first look: it takes note
	clock.settle(t)
	eventually(t, "the note", func() bool { st, _ := store.Status(); return st["inbox"].Seen != "" && !s.busy("inbox") })
	if err := s.RunNow("inbox"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the run", func() bool { return rec.kinds("inbox", Finished) == 1 && !s.busy("inbox") })
	clock.advance(time.Minute)
	clock.settle(t)
	time.Sleep(200 * time.Millisecond)
	if n := rec.count(); n != 1 {
		t.Fatalf("%d runs: the run by hand set it off", n)
	}
	if _, err := os.Stat(rec.write); err != nil {
		t.Fatal(err)
	}
}

func TestRunNowRefusedDoesNotRebaseFile(t *testing.T) {
	dir := t.TempDir()
	tr := Trigger{Name: "inbox", Kind: File, Path: dir, When: "@every 1m", Prompt: "x"}
	store, clock, rec := harness(t, tr)
	var allow atomic.Bool
	s := launchHooked(t, store, clock, rec, Config{BeforeRun: func(_ context.Context, _ Trigger, f Fired) (Fired, error) {
		if !allow.Load() {
			return f, stderrors.New("refused")
		}
		return f, nil
	}})
	clock.advance(time.Minute)
	clock.settle(t)
	eventually(t, "initial note", func() bool { st, _ := store.Status(); return st["inbox"].Seen != "" })
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.RunNow("inbox"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "refused", func() bool { return rec.kinds("inbox", Refused) == 1 && !s.busy("inbox") })
	allow.Store(true)
	clock.advance(time.Minute)
	clock.settle(t)
	eventually(t, "file still detected", func() bool { return rec.kinds("inbox", Finished) == 1 })
}
