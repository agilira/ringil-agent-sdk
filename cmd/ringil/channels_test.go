// channels_test.go: each chat's documents to its own runs only
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
)

func TestInboxOf(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "telegram-5"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "telegram-6")); err != nil {
		t.Fatal(err)
	}
	c := &cli{inboxRoot: root}
	if got := c.inboxOf("telegram-5"); got != filepath.Join(root, "telegram-5") {
		t.Fatalf("own inbox %q", got)
	}
	for _, conv := range []string{"", "telegram-7", "telegram-6", "../x", "."} {
		if got := c.inboxOf(conv); got != "" {
			t.Errorf("%q granted %q", conv, got)
		}
	}
	if got := (&cli{}).inboxOf("telegram-5"); got != "" {
		t.Fatalf("without channels: %q", got)
	}
}

func TestNewAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{"24h": 24 * time.Hour, "90m": 90 * time.Minute, "never": -1} {
		if got, err := newAfter(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "30s", "-1h", "soon"} {
		if _, err := newAfter(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMemoryDir(t *testing.T) {
	user := "/cfg/ringil/memory"
	for name, c := range map[string]struct {
		cli  *cli
		want string
		ok   bool
	}{
		"user":          {&cli{}, user, true},
		"role, shared":  {&cli{role: &profile{}}, user, true},
		"off":           {&cli{role: &profile{Memory: "off"}, runConversation: "telegram-5"}, "", false},
		"chat":          {&cli{role: &profile{Memory: "chat"}, runConversation: "telegram-5_3"}, filepath.Join(user, "chats", "telegram-5"), true},
		"chat, none":    {&cli{role: &profile{Memory: "chat"}}, "", false},
		"chat, invalid": {&cli{role: &profile{Memory: "chat"}, runConversation: "../x"}, "", false},
	} {
		got, ok := c.cli.memoryDir(user)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: %q %v", name, got, ok)
		}
	}
}

func TestOnlyTools(t *testing.T) {
	reg, err := builtinTools()
	if err != nil {
		t.Fatal(err)
	}
	none, some, bad := []string{}, []string{"now"}, []string{"now", "bash"}
	if got, _ := (&cli{}).onlyTools(reg); len(got.Specs()) != 2 {
		t.Fatal("no role: every tool")
	}
	if got, _ := (&cli{role: &profile{Tools: &some}}).onlyTools(reg); len(got.Specs()) != 1 || !got.Has("now") {
		t.Fatal("listed: only those")
	}
	if got, _ := (&cli{role: &profile{Tools: &none}}).onlyTools(reg); len(got.Specs()) != 0 {
		t.Fatal("empty list: none")
	}
	if _, err := (&cli{role: &profile{Tools: &bad}}).onlyTools(reg); err == nil {
		t.Fatal("a tool the role does not have")
	}
}

func TestCheckChatProfile(t *testing.T) {
	none := []string{}
	roles := map[string]profile{
		"team":  {Tools: &none, Memory: "chat"},
		"open":  {Memory: "chat"},
		"mine":  {Tools: &none},
		"quiet": {Tools: &none, Memory: "off"},
	}
	for name, ok := range map[string]bool{"": true, "team": true, "quiet": true, "open": false, "mine": false, "default": false, "missing": false} {
		if err := checkChatProfile(roles, name); (err == nil) != ok {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func TestRestrictedRoleHasNoArtifacts(t *testing.T) {
	reg, err := builtinTools()
	if err != nil {
		t.Fatal(err)
	}
	publish := agent.NewTool("publish_artifact", "stands in for the real one", func(context.Context, struct{}) (string, error) { return "", nil })
	if reg, err = reg.With(publish); err != nil {
		t.Fatal(err)
	}
	list := []string{"now", "publish_artifact"}
	if _, err := (&cli{role: &profile{Tools: &list}}).onlyTools(reg); err == nil {
		t.Fatal("a restricted role with the user's artifacts")
	}
}
