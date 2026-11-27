// apphooks_test.go: the user's hooks, as commands
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// script writes an executable shell script and returns its path.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func hooksYAML(entries ...string) string {
	return "models: [openai:m]\nhooks:\n" + strings.Join(entries, "")
}

func hook(event, path string, extra ...string) string {
	return "  " + event + ":\n    - command: [" + path + "]\n" + strings.Join(extra, "")
}

// lastText is the text of a request's last message; lastResult its
// tool result.
func lastText(req *agent.Request) string { return req.Messages[len(req.Messages)-1].Text() }

func lastResult(req *agent.Request) *agent.ToolResult {
	return req.Messages[len(req.Messages)-1].Result
}

func fileIn(t *testing.T, path string, want ...string) string {
	t.Helper()
	data, _ := os.ReadFile(path) // #nosec G304 -- the test's own file
	for _, w := range want {
		if !strings.Contains(string(data), w) {
			t.Fatalf("%s lacks %q: %s", filepath.Base(path), w, data)
		}
	}
	return string(data)
}

// before_run changes what the model reads; before_tool blocks a call with
// its reason; after_tool changes a result; after_run is told. Each reads
// the event as JSON, without the process's keys.
func TestHooksAroundARun(t *testing.T) {
	h := newHarness(t, "", fake.Call("todo_write", `{"todos":[]}`), fake.Call("todo_write", `{"todos":[{"content":"x","status":"pending"}]}`), fake.Reply("done"))
	bin := t.TempDir()
	seen := filepath.Join(bin, "seen")
	before := script(t, bin, "before", `cat > `+seen+`.run; env >> `+seen+`.env; echo '{"text":"rewritten by the hook"}'`)
	block := script(t, bin, "block", `cat > `+seen+`.tool; grep -q '"todos":\[\]' `+seen+`.tool && { echo "empty lists are not allowed" >&2; exit 2; }; exit 0`)
	after := script(t, bin, "after", `cat > /dev/null; echo '{"content":"[checked]"}'`)
	told := script(t, bin, "told", `cat > `+seen+`.end`)
	writeConfig(t, h.dir, hooksYAML(hook("before_run", before), hook("before_tool", block, "      tools: [todo_write]\n"),
		hook("after_tool", after), hook("after_run", told)))
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "sk-secret-key", "PATH": os.Getenv("PATH")})
	if code := h.run("--no-approvals", "run", "the original"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	reqs := h.script.Requests()
	if got := lastText(reqs[0]); !strings.Contains(got, "rewritten by the hook") || strings.Contains(got, "the original") {
		t.Fatalf("the model read %q", got)
	}
	if res := lastResult(reqs[1]); res == nil || !res.IsError || !strings.Contains(res.Content, "empty lists are not allowed") {
		t.Fatalf("the blocked call answered %+v", res)
	}
	if res := lastResult(reqs[2]); res == nil || res.Content != "[checked]" {
		t.Fatalf("after_tool did not change the result: %+v", res)
	}
	hooksRead(t, seen)
}

// hooksRead: each hook read its event, and none saw the process's key.
func hooksRead(t *testing.T, seen string) {
	t.Helper()
	fileIn(t, seen+".run", `"event":"before_run"`, "the original")
	if strings.Contains(fileIn(t, seen+".env", "RINGIL_HOOK_EVENT=before_run"), "sk-secret-key") {
		t.Fatal("a hook saw the key")
	}
	fileIn(t, seen+".end", `"event":"after_run"`, `"text":"done"`)
}

// A hook that fails goes on by default, and blocks with on_error: block;
// one past its timeout is stopped.
func TestHookFailures(t *testing.T) {
	bin := t.TempDir()
	crash := script(t, bin, "crash", "exit 1")
	slow := script(t, bin, "slow", "sleep 30")
	for name, c := range map[string]struct {
		entry string
		ok    bool
	}{
		"goes on":  {hook("before_run", crash), true},
		"blocks":   {hook("before_run", crash, "      on_error: block\n"), false},
		"too slow": {hook("before_run", slow, "      timeout: 1s\n      on_error: block\n"), false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "", fake.Reply("ok"))
			writeConfig(t, h.dir, hooksYAML(c.entry))
			env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "k", "PATH": os.Getenv("PATH")})
			code := h.run("--no-tools", "run", "x")
			if (code == exitOK) != c.ok {
				t.Fatalf("exit %d: %s", code, h.err)
			}
			if !strings.Contains(h.err.String(), "hook before_run") {
				t.Fatalf("the failure was not told: %s", h.err)
			}
		})
	}
}

// Hooks are checked with the file: a command by a relative path, a bad
// timeout, tools on an event without tools, are refused.
func TestHooksAreChecked(t *testing.T) {
	for _, bad := range []hooksConfig{
		{BeforeRun: []hookEntry{{}}},
		{BeforeRun: []hookEntry{{Command: []string{"bin/check"}}}},
		{BeforeRun: []hookEntry{{Command: []string{"check"}, Timeout: "1h"}}},
		{BeforeRun: []hookEntry{{Command: []string{"check"}, Tools: []string{"bash"}}}},
		{BeforeTool: []hookEntry{{Command: []string{"check"}, OnError: "maybe"}}},
		{BeforeTool: []hookEntry{{Command: []string{"check"}, Tools: []string{"no tool"}}}},
	} {
		if err := bad.check(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if err := (hooksConfig{BeforeTool: []hookEntry{{Command: []string{"/usr/bin/check", "--strict"}, Tools: []string{"bash"}, Timeout: "5s", OnError: "block"}}}).check(); err != nil {
		t.Fatal(err)
	}
}

// A hook that leaves a child holding its output answers all the same, at
// once, and the child is ended with it.
func TestHookChildIsEnded(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	bin := t.TempDir()
	mark := filepath.Join(bin, "child-lived")
	child := script(t, bin, "child", `(sleep 1; touch `+mark+`) & echo '{"text":"rewritten by the hook"}'`)
	writeConfig(t, h.dir, hooksYAML(hook("before_run", child)))
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "k", "PATH": os.Getenv("PATH")})
	if code := h.run("--no-tools", "run", "the original"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if got := lastText(h.script.Requests()[0]); !strings.Contains(got, "rewritten by the hook") {
		t.Fatalf("the model read %q", got)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the hook's child outlived it")
	}
}
