// security_test.go: fuzz and injection/tampering security tests for skills
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
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

func FuzzParseSkill(f *testing.F) {
	f.Add("---\nname: triage\ndescription: Sort mail\n---\n\nDo step 1\n", "triage", "Sort mail", "Do step 1")
	f.Add("\ufeff---\r\nname: a-b\r\ndescription: |\r\n  line 1\r\n  line 2\r\n---\r\nbody", "a-b", "line 1", "body")
	f.Add("---\nname: x\nname: y\ndescription: dup\n---\nbody", "x", "dup\u200b", "body")
	f.Add("---\nname: x\ndescription: >-\n  folded\n  line\n---\n---\nstill body", "x", `evil"\nname: "hijacked`, "---\nbody")
	f.Add("---\nname: x\ndescription: 'unclosed", "true", "quoted 'x' and \"y\"", "do\u202ethis")

	f.Fuzz(func(t *testing.T, raw, name, desc, instr string) {
		if len(raw) > 4096 || len(name) > 256 || len(desc) > 2048 || len(instr) > 4096 {
			return
		}
		checkParsedSkill(t, raw)
		checkCreateRoundTrip(t, Info{Name: name, Description: desc}, instr)
	})
}

func checkParsedSkill(t *testing.T, raw string) {
	t.Helper()
	info, _, err := parse(raw)
	if err != nil {
		return
	}
	if !validName(info.Name) || strings.TrimSpace(info.Description) == "" {
		t.Fatalf("parse returned invalid info: %+v", info)
	}
	if !utf8.ValidString(info.Description) || utf8.RuneCountInString(info.Description) > 1024 {
		t.Fatalf("parse returned bad description: %+v", info)
	}
}

func checkCreateRoundTrip(t *testing.T, in Info, instr string) {
	t.Helper()
	if checkCreate(in, instr) != nil {
		return
	}
	got, body, err := parse(skillText(in, instr))
	if err != nil {
		t.Fatalf("round-trip failed for %+v: %v", in, err)
	}
	if got.Name != in.Name || got.Description != strings.TrimSpace(in.Description) || body != strings.TrimSpace(instr) {
		t.Fatalf("round-trip mismatch: got=(%+v, %q) want=(%+v, %q)", got, body, in, strings.TrimSpace(instr))
	}
}

func TestSecurityCreateBlocksHiddenCharsAndInjection(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	badInputs := []struct {
		name string
		info Info
		body string
	}{
		{"bidiOverride", Info{Name: "a", Description: "ok"}, "step \u202e evil"},
		{"bidiIsolate", Info{Name: "a", Description: "ok"}, "step \u2066 evil"},
		{"zeroWidth", Info{Name: "a", Description: "ok\u200b"}, "step"},
		{"bom", Info{Name: "a", Description: "ok"}, "\ufeffstep"},
		{"ansiEscape", Info{Name: "a", Description: "ok"}, "step\x1b[31m"},
		{"nulByte", Info{Name: "a", Description: "ok"}, "step\x00"},
		{"lineSep", Info{Name: "a", Description: "ok"}, "step\u2028"},
		{"paraSep", Info{Name: "a", Description: "ok"}, "step\u2029"},
		{"multilineDesc", Info{Name: "a", Description: "one\ntwo"}, "step"},
		{"crDesc", Info{Name: "a", Description: "one\rtwo"}, "step"},
		{"oversized", Info{Name: "a", Description: "ok"}, strings.Repeat("x", MaxInstructions+1)},
		{"invalidUTF8", Info{Name: "a", Description: "ok"}, "bad\xff"},
	}
	for _, c := range badInputs {
		if err := Create(ctx, dir, c.info, c.body, false); err == nil {
			t.Errorf("%s: accepted by Create", c.name)
		}
	}
	injections := []struct{ name, desc string }{
		{"123", `evil"\nname: "hijacked`},
		{"true", `---`},
	}
	for _, inj := range injections {
		info := Info{Name: inj.name, Description: inj.desc}
		if err := Create(ctx, dir, info, "body\n---\nextra", false); err != nil {
			t.Fatalf("%s create: %v", inj.name, err)
		}
	}
	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, inj := range injections {
		body, err := cat.loadForModel(inj.name)
		if err != nil || !strings.Contains(body, "extra") {
			t.Fatalf("%s load: %q %v", inj.name, body, err)
		}
	}
}

func TestSecurityCreateCannotOverwriteUserOrScriptedSkill(t *testing.T) {
	dir := t.TempDir()
	src := t.TempDir()
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: user-skill\ndescription: owned by user\n---\nkeep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(ctx, src, dir); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, dir, Info{Name: "user-skill", Description: "hijack"}, "evil", true); !errors.HasCode(err, ErrExists) {
		t.Fatalf("overwrote user skill: %v", err)
	}
	if err := Create(ctx, dir, Info{Name: "agent-skill", Description: "mine"}, "step", false); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "agent-skill", "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-skill", "scripts", "run.sh"), []byte("id"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, dir, Info{Name: "agent-skill", Description: "mine"}, "new", true); !errors.HasCode(err, ErrExists) {
		t.Fatalf("overwrote skill with scripts dir: %v", err)
	}
}
