// create_test.go: skills the agent writes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateThenLoad(t *testing.T) {
	dir := t.TempDir()
	info := Info{Name: "triage-mail", Description: `Sort the mail: "urgent" first`}
	if err := Create(context.Background(), dir, info, "1. Read the inbox.\n2. Flag what is urgent.", false); err != nil {
		t.Fatal(err)
	}
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l := c.List(); len(l) != 1 || l[0] != info {
		t.Fatalf("%+v", l)
	}
	body, err := c.loadForModel("triage-mail")
	if err != nil || !strings.Contains(body, "Flag what is urgent.") {
		t.Fatalf("%q %v", body, err)
	}
}

func TestCreateRefuses(t *testing.T) {
	dir := t.TempDir()
	ok := Info{Name: "a-skill", Description: "does a"}
	for name, c := range map[string]struct {
		info Info
		body string
	}{
		"bad name":        {Info{Name: "A Skill", Description: "x"}, "do"},
		"two-line desc":   {Info{Name: "a", Description: "x\nname: evil"}, "do"},
		"no instructions": {ok, "  "},
		"too long":        {ok, strings.Repeat("x", MaxInstructions+1)},
	} {
		if err := Create(context.Background(), dir, c.info, c.body, false); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := Create(context.Background(), dir, ok, "do a", false); err != nil {
		t.Fatal(err)
	}
	if err := Create(context.Background(), dir, ok, "do a again", false); err == nil {
		t.Fatal("overwrote without replace")
	}
}

func TestReplaceOnlyInstructions(t *testing.T) {
	dir := t.TempDir()
	ok := Info{Name: "a-skill", Description: "does a"}
	if err := Create(context.Background(), dir, ok, "first", false); err != nil {
		t.Fatal(err)
	}
	if err := Create(context.Background(), dir, ok, "second", true); err != nil {
		t.Fatal(err)
	}
	c, _ := Open(dir)
	if body, _ := c.loadForModel("a-skill"); !strings.Contains(body, "second") {
		t.Fatalf("%q", body)
	}
	// a skill with a script is the user's
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: scripted\ndescription: has a script\n---\nrun it"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "run.sh"), []byte("echo hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), src, dir); err != nil {
		t.Fatal(err)
	}
	if err := Create(context.Background(), dir, Info{Name: "scripted", Description: "x"}, "y", true); err == nil {
		t.Fatal("replaced a skill with a script")
	}
	if _, err := os.Stat(filepath.Join(dir, "scripted", "run.sh")); err != nil {
		t.Fatal("the user's skill was touched")
	}
}

func TestWhatDoesNotShowIsRefused(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ desc, body string }{
		{"sort \U000E0041\U000E0042", "do"},
		{"sort", "do\u200b this"},
		{"sort", "do \u202e this"},
		{"sort", "do\x1b[2J"},
	} {
		if err := Create(context.Background(), dir, Info{Name: "a", Description: c.desc}, c.body, false); err == nil {
			t.Errorf("%q / %q written", c.desc, c.body)
		}
	}
	if err := Create(context.Background(), dir, Info{Name: "a", Description: "ok"}, "line\n\ttabbed", false); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyTheAgentsSkillsAreReplaced(t *testing.T) {
	dir := t.TempDir()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: mine\ndescription: the user's\n---\nmine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), src, dir); err != nil {
		t.Fatal(err)
	}
	if err := Create(context.Background(), dir, Info{Name: "mine", Description: "x"}, "y", true); err == nil {
		t.Fatal("replaced the user's skill")
	}
	if err := Create(context.Background(), dir, Info{Name: "ours", Description: "x"}, "y", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ours", "run.sh"), []byte("echo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Create(context.Background(), dir, Info{Name: "ours", Description: "x"}, "z", true); err == nil {
		t.Fatal("replaced a skill holding a file of the user's")
	}
}

func TestNamesThatYAMLWouldReadAsOtherThings(t *testing.T) {
	dir := t.TempDir()
	if err := Create(context.Background(), dir, Info{Name: "true", Description: "x"}, "y", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "true", "SKILL.md"))
	if err != nil || !strings.Contains(string(data), `name: "true"`) {
		t.Fatalf("%s %v", data, err)
	}
	c, _ := Open(dir)
	if l := c.List(); len(l) != 1 || l[0].Name != "true" {
		t.Fatalf("%+v", l)
	}
}

func TestAnInterruptedReplaceIsPutBack(t *testing.T) {
	dir := t.TempDir()
	if err := Create(context.Background(), dir, Info{Name: "a", Description: "x"}, "y", false); err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(dir, replacedPrefix+"old")
	if err := os.Rename(filepath.Join(dir, "a"), aside); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(aside, past, past); err != nil {
		t.Fatal(err)
	}
	if err := Create(context.Background(), dir, Info{Name: "b", Description: "x"}, "y", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a", "SKILL.md")); err != nil {
		t.Fatal("not put back")
	}
}
