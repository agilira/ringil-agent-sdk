// jobs_test.go: background commands end checked, and say why they failed
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
)

// What a background command plants in a repository is undone before git
// can run on the host, while the command still runs, and when the shell
// closes; Close stops it and refuses more (audit 21/11: the check came
// only at the job's end, and nothing ended the job).
func TestBackgroundJobsAreGuardedAndClosed(t *testing.T) {
	box := boxOrSkip(t)
	dir := t.TempDir()
	sh, err := New(Config{Dir: dir, Sandbox: box, Read: []string{},
		Confirm: func(context.Context, Request) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	tools := sh.Tools()
	bash := tools[0]
	out, err := call(t, bash, map[string]any{"background": true,
		"command": `mkdir -p r/.git/hooks && printf '#!/bin/sh\necho evil\n' > r/.git/hooks/pre-commit && sleep 300`})
	if err != nil || !strings.Contains(out, "job b1") {
		t.Fatalf("%q %v", out, err)
	}
	hook := filepath.Join(dir, "r/.git/hooks/pre-commit")
	waitFor(t, hook)
	out, err = call(t, bash, map[string]any{"command": "echo host", "unsandboxed": true, "reason": "test"})
	if err != nil || !strings.Contains(out, "host") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := os.Stat(hook); err == nil {
		t.Fatalf("the hook was there when git could run on the host: %q", out)
	}
	closesAndRefuses(t, sh, bash)
}

// closesAndRefuses: Close stops the job at once, and no command runs
// after it.
func closesAndRefuses(t *testing.T, sh *Shell, bash agent.Tool) {
	t.Helper()
	start := time.Now()
	if err := sh.Close(); err != nil || time.Since(start) > 10*time.Second {
		t.Fatalf("close: %v after %v", err, time.Since(start))
	}
	if _, err := call(t, bash, map[string]any{"command": "echo late"}); err == nil {
		t.Fatal("a closed shell ran a command")
	}
}

// waitFor waits until path exists.
func waitFor(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatal("the job did not plant its hook")
}

// A background command that cannot start says why (audit 21/11: it read
// "Ended after 0s." and nothing else).
func TestBackgroundStartFailureIsReported(t *testing.T) {
	sh, err := New(Config{Dir: t.TempDir(), Sandbox: noBox{}, Read: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Close() })
	tools := sh.Tools()
	if _, err := call(t, tools[0], map[string]any{"command": "true", "background": true}); err != nil {
		t.Fatal(err)
	}
	var out string
	for range 100 {
		out, _ = call(t, tools[1], map[string]any{"id": "b1"})
		if strings.HasPrefix(out, "Ended") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out, "could not start") {
		t.Fatalf("%q", out)
	}
}
