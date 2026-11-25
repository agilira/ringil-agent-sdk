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

// An archive entry may not leave the download folder, whatever its path.
func TestInstallGitHubArchiveEscapes(t *testing.T) {
	for _, name := range []string{"../evil", "a/../../evil", "/etc/evil", "C:/evil", "a\\..\\..\\evil"} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, archive(t, file("SKILL.md", skillMD("a")), file(name, "x")))
			_, _, err := installLink(t, f, "o/r")
			if !errors.HasCode(err, ErrInvalid) {
				t.Fatalf("err %v", err)
			}
		})
	}
	// Under a folder link, the folder's own prefix cannot be climbed out of.
	f := newFake(t, archive(t, file("s/SKILL.md", skillMD("a")), file("s/../../evil", "x")))
	if _, _, err := installLink(t, f, "o/r/s@main"); !errors.HasCode(err, ErrInvalid) {
		t.Fatalf("err %v", err)
	}
}

// Too many files, too many bytes, a download over its bound: refused
// before anything is installed.
func TestInstallGitHubBounds(t *testing.T) {
	big := archive(t, file("SKILL.md", skillMD("a")), file("blob.bin", strings.Repeat("x", 4096)))
	many := []entry{file("SKILL.md", skillMD("a"))}
	for i := range 20 {
		many = append(many, file("f"+string(rune('a'+i)), "x"))
	}
	for _, c := range []struct {
		name string
		data []byte
		set  func(*GitHub)
	}{
		{"bytes", big, func(g *GitHub) { g.MaxBytes = 1024 }},
		{"files", archive(t, many...), func(g *GitHub) { g.MaxFiles = 10 }},
		{"download", big, func(g *GitHub) { g.MaxDownload = 64 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t, c.data)
			g := f.gh()
			c.set(&g)
			dir := filepath.Join(t.TempDir(), "skills")
			if _, err := InstallGitHub(bg, "o/r", dir, g); err == nil {
				t.Fatal("installed")
			}
			if entries, _ := os.ReadDir(dir); len(entries) > 0 {
				t.Fatalf("left %v", entries)
			}
		})
	}
}

// A highly compressed entry (a megabyte of zeros) is refused by its
// declared size before it is written.
func TestInstallGitHubDeclaredSizeIsTheBound(t *testing.T) {
	f := newFake(t, archive(t, file("SKILL.md", skillMD("a")), file("zeros", strings.Repeat("\x00", 1<<20))))
	g := f.gh()
	g.MaxBytes = 1 << 19
	if _, err := InstallGitHub(bg, "o/r", filepath.Join(t.TempDir(), "s"), g); !errors.HasCode(err, ErrFetch) {
		t.Fatalf("err %v", err)
	}
}

func FuzzParseLink(f *testing.F) {
	for _, s := range []string{
		"https://github.com/o/r/tree/main/a/b", "o/r/a@v1", "github.com/o/r/blob/x/SKILL.md",
		"o/r/../x", "https://github.com/o/r/tree/main/%2e%2e", "o/r@a/../b", "o/r/a\\b", "o/r/\x00",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, link string) {
		if r, err := parseLink(link, "github.com"); err == nil {
			checkParsedLink(t, link, r)
		}
	})
}

func checkParsedLink(t *testing.T, link string, r ghRef) {
	t.Helper()
	if !ghOwner.MatchString(r.owner) || !ghName.MatchString(r.repo) || r.repo == ".." {
		t.Fatalf("%q: repository %q/%q", link, r.owner, r.repo)
	}
	if r.path != "" && !fsLocal(r.path) {
		t.Fatalf("%q: path %q escapes", link, r.path)
	}
	if strings.Contains(r.ref, "..") || strings.ContainsAny(r.ref, " \\\x00") {
		t.Fatalf("%q: ref %q", link, r.ref)
	}
}
