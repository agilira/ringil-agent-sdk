// approvals_test.go: --no-approvals
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/notes"
	"github.com/agilira/ringil-agent-sdk/tools/bash"
)

func TestNoApprovalsAsksNothing(t *testing.T) {
	var out bytes.Buffer
	c := &cli{in: strings.NewReader(""), out: &out, errOut: &out, noApprovals: true}
	ctx := context.Background()
	destructive := agent.ToolSpec{Annotations: agent.Annotations{Destructive: true}}
	checks := map[string]func() (bool, error){
		"tool":      func() (bool, error) { return c.approve(ctx, agent.ToolCall{Name: "wipe"}, destructive) },
		"command":   func() (bool, error) { return c.confirmCommand(ctx, bash.Request{Command: "rm -rf build"}) },
		"domain":    func() (bool, error) { return c.askDomain(ctx, "pypi.org") },
		"protected": func() (bool, error) { return c.confirmProtected(ctx, ".git/config") },
	}
	for name, check := range checks {
		if ok, err := check(); !ok || err != nil {
			t.Errorf("%s: %v %v", name, ok, err)
		}
	}
	if out.Len() > 0 {
		t.Fatalf("asked: %q", out.String())
	}
}

func TestNoApprovalsLeavesChatsToTheBridge(t *testing.T) {
	c := &cli{noApprovals: true, runConversation: "telegram-42"}
	if c.unasked() {
		t.Fatal("a chat's run went unasked: the Bridge decides by who started it")
	}
	c.runConversation = ""
	if !c.unasked() {
		t.Fatal("asked")
	}
}

func TestEveryChatIsLeftToTheBridge(t *testing.T) {
	for _, name := range chats {
		c := &cli{noApprovals: true, runConversation: name + "-42"}
		if c.unasked() || c.chatSystem("x") == "x" {
			t.Errorf("%s", name)
		}
	}
}

// approve is decide's yes or no, for the tests.
func (c *cli) approve(ctx context.Context, call agent.ToolCall, spec agent.ToolSpec) (bool, error) {
	d, err := c.decide(ctx, call, spec)
	return d.Allow, err
}

// 23/11: ringil.yaml's permissions, as coding agents have them: rules
// before the mode, deny first; plan mode refuses changes; "p" keeps the
// rule in ringil.yaml, "a" for the session only.
func TestPermissionsFromTheFile(t *testing.T) {
	edit := agent.ToolSpec{Name: "write_file", Annotations: agent.Annotations{Edits: true}, InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`)}
	risky := agent.ToolSpec{Name: "forget", Annotations: agent.Annotations{Destructive: true}}
	for body, want := range map[string]map[string]bool{
		"permissions: {allow: [\"write_file(src/**)\"], deny: [forget]}\n": {"src/a.go": true, "doc/b.md": false, "forget": false},
		"permissions: {mode: plan}\n":                                      {"src/a.go": false, "forget": false},
		"permissions: {mode: accept-edits}\n":                              {"src/a.go": true, "forget": false},
	} {
		h := newHarness(t, strings.Repeat("n\n", 5))
		writeConfig(t, h.dir, body)
		loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
			for subject, allow := range want {
				c, s := agent.ToolCall{Name: "write_file", Input: []byte(`{"path":"` + subject + `"}`)}, edit
				if subject == "forget" {
					c, s = agent.ToolCall{Name: "forget", Input: []byte(`{}`)}, risky
				}
				if ok, _ := h.c.approve(ctx.Context(), c, s); ok != allow {
					t.Errorf("%q %s: %v", body, subject, ok)
				}
			}
			return nil
		})
	}
	if err := (permissionsConfig{Mode: "yolo"}).check(); err == nil {
		t.Error("an unknown mode")
	}
	if err := (permissionsConfig{Deny: []string{"bash("}}).check(); err == nil {
		t.Error("a broken rule")
	}
}

func TestAlwaysIsKept(t *testing.T) {
	h := newHarness(t, "p\na\n")
	path := writeConfig(t, h.dir, "# mine\nmodel: m\n")
	edit := agent.ToolSpec{Name: "write_file", Annotations: agent.Annotations{Edits: true}, InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`)}
	risky := agent.ToolSpec{Name: "forget", Annotations: agent.Annotations{Destructive: true}}
	calls := []struct {
		call agent.ToolCall
		spec agent.ToolSpec
	}{
		{agent.ToolCall{Name: "write_file", Input: []byte(`{"path":"src/x0.go"}`)}, edit},
		{agent.ToolCall{Name: "write_file", Input: []byte(`{"path":"src/x1.go"}`)}, edit},
		{agent.ToolCall{Name: "forget", Input: []byte(`{}`)}, risky},
		{agent.ToolCall{Name: "forget", Input: []byte(`{}`)}, risky},
	}
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		for _, c := range calls {
			if ok, err := h.c.approve(ctx.Context(), c.call, c.spec); !ok || err != nil {
				t.Fatalf("%s: %v %v", c.call.Input, ok, err)
			}
		}
		return nil
	})
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "write_file(src/**)") || strings.Contains(string(data), "forget") || !strings.Contains(string(data), "# mine") {
		t.Fatalf("%s", data)
	}
	if n := strings.Count(h.err.String()+h.out.String(), "wants to run"); n != 2 {
		t.Fatalf("asked %d times, want 2 (once each)", n)
	}
}

// Audit 23/11: an automation allowed bash still runs no command a deny rule
// names, nor one whose program an expansion hides.
func TestATriggerHonoursTheRules(t *testing.T) {
	h := newHarness(t, "")
	writeConfig(t, h.dir, "permissions: {deny: [\"bash(rm:*)\"]}\n")
	h.c.inTrigger, h.c.triggerAllow = true, []string{"bash"}
	bashTool := agent.ToolSpec{Name: "bash", Annotations: agent.Annotations{Destructive: true, SelfConfirms: true},
		InputSchema: []byte(`{"type":"object","properties":{"command":{"type":"string"}}}`)}
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		for command, want := range map[string]bool{"ls -la": true, "rm -rf $HOME": false, "$(printf r)m x": false, "r''m x": false} {
			input, _ := json.Marshal(map[string]string{"command": command})
			if ok, _ := h.c.approve(ctx.Context(), agent.ToolCall{Name: "bash", Input: input}, bashTool); ok != want {
				t.Errorf("%q: %v", command, ok)
			}
		}
		return nil
	})
}

func TestRememberProjectFailureStillAllowsSession(t *testing.T) {
	h := newHarness(t, "p\n")
	path := writeConfig(t, h.dir, "model: m\n")
	edit := agent.ToolSpec{Name: "write_file", Annotations: agent.Annotations{Edits: true}, InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`)}
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		if err := os.WriteFile(path, []byte("[\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"src/a.go", "src/b.go"} {
			if ok, err := h.c.approve(ctx.Context(), agent.ToolCall{Name: "write_file", Input: []byte(`{"path":"` + p + `"}`)}, edit); !ok || err != nil {
				t.Fatalf("%s: %v %v", p, ok, err)
			}
		}
		return nil
	})
	out := h.err.String() + h.out.String()
	if !strings.Contains(out, "this session only") || strings.Count(out, "wants to run") != 1 {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestNotesFollowPermissionsRules(t *testing.T) {
	nb, err := notes.New(notes.Scope{Name: "project", Dir: t.TempDir(), ConfirmChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	spec := nb.Tool().Spec()
	hAsk := newHarness(t, "n\n")
	writeConfig(t, hAsk.dir, "permissions: {ask: [notes]}\n")
	loaded(t, hAsk.c, nil, func(ctx *orpheus.Context) error {
		hAsk.c.notes = nb
		if ok, _ := hAsk.c.approve(ctx.Context(), agent.ToolCall{Name: notes.DefaultName, Input: []byte(`{"command":"view","path":"/project"}`)}, spec); ok {
			t.Fatal("permissions.ask did not ask for notes view")
		}
		return nil
	})
	hAllow := newHarness(t, "")
	writeConfig(t, hAllow.dir, "permissions: {allow: [notes]}\n")
	loaded(t, hAllow.c, nil, func(ctx *orpheus.Context) error {
		hAllow.c.notes = nb
		if ok, err := hAllow.c.approve(ctx.Context(), agent.ToolCall{Name: notes.DefaultName, Input: []byte(`{"command":"create","path":"/project/a.md","file_text":"x"}`)}, spec); !ok || err != nil {
			t.Fatalf("permissions.allow did not allow project note: %v %v", ok, err)
		}
		return nil
	})
}

// Plan mode reads: the notes tool's views run, its changes do not, and a
// deny rule on notes still refuses.
func TestNotesReadInPlanMode(t *testing.T) {
	nb, err := notes.New(notes.Scope{Name: "project", Dir: t.TempDir(), ConfirmChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	spec := nb.Tool().Spec()
	view := agent.ToolCall{Name: notes.DefaultName, Input: []byte(`{"command":"view","path":"/project"}`)}
	create := agent.ToolCall{Name: notes.DefaultName, Input: []byte(`{"command":"create","path":"/project/a.md","file_text":"x"}`)}
	for body, want := range map[string][2]bool{
		"permissions: {mode: plan}\n":                {true, false},
		"permissions: {mode: plan, deny: [notes]}\n": {false, false},
	} {
		h := newHarness(t, "y\ny\n")
		writeConfig(t, h.dir, body)
		loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
			h.c.notes = nb
			for i, c := range []agent.ToolCall{view, create} {
				if ok, _ := h.c.approve(ctx.Context(), c, spec); ok != want[i] {
					t.Errorf("%q %s: %v", body, c.Input, ok)
				}
			}
			return nil
		})
	}
}
