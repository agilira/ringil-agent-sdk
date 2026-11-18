// skills_test.go: parsing, installing, verifying and using skills
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

	"github.com/agilira/go-errors"
)

var bg = context.Background()

const reviewSkill = `---
name: review
description: >
  Review files and report concrete findings.
  Use when asked to review.
license: MIT
metadata:
  author: someone
---

# Review

Run scripts/stats.py on the text, then report.
`

// newSkill writes a skill directory and returns its path.
func newSkill(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func installed(t *testing.T) (*Catalog, string) {
	t.Helper()
	src := newSkill(t, map[string]string{
		"SKILL.md":           reviewSkill,
		"scripts/stats.py":   "import sys\nt=sys.stdin.read()\nprint(len(t.split()), 'words', sys.argv[1:])\n",
		"reference/style.md": "Be concrete.",
		".git/config":        "hidden",
	})
	cat := filepath.Join(t.TempDir(), "skills")
	info, err := Install(bg, src, cat)
	if err != nil || info.Name != "review" {
		t.Fatalf("install %v %v", info, err)
	}
	c, err := Open(cat)
	if err != nil {
		t.Fatal(err)
	}
	return c, cat
}

func TestParseFrontMatter(t *testing.T) {
	info, body, err := parse(reviewSkill)
	if err != nil {
		t.Fatal(err)
	}
	if info.Description != "Review files and report concrete findings. Use when asked to review." || !strings.HasPrefix(body, "# Review") {
		t.Fatalf("%+v %q", info, body)
	}
	for _, bad := range []string{
		"no front matter",
		"---\nname: x\n",
		"---\nname: Bad_Name\ndescription: d\n---\nbody",
		"---\nname: ok\n---\nbody",
		"---\nname: -x\ndescription: d\n---\n",
	} {
		if _, _, err := parse(bad); !errors.HasCode(err, ErrInvalid) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	quoted, _, err := parse("---\r\nname: \"q\"\r\ndescription: 'quoted: yes'\r\n---\r\nb")
	if err != nil || quoted.Name != "q" || quoted.Description != "quoted: yes" {
		t.Fatalf("quoted %+v %v", quoted, err)
	}
}

func TestInstallLoadRead(t *testing.T) {
	c, _ := installed(t)
	if l := c.List(); len(l) != 1 || l[0].Name != "review" {
		t.Fatalf("list %v", l)
	}
	files, err := c.Files("review")
	if err != nil || strings.Join(files, ",") != "SKILL.md,reference/style.md,scripts/stats.py" {
		t.Fatalf("files %v %v (hidden entries must be left out)", files, err)
	}
	body, err := c.Load("review")
	if err != nil || !strings.HasPrefix(body, "# Review") {
		t.Fatalf("load %q %v", body, err)
	}
	if !strings.Contains(c.Instructions(), `{"name":"review","description":"Review files`) {
		t.Fatalf("instructions %q", c.Instructions())
	}
}

func TestReadOnlyListedFiles(t *testing.T) {
	c, _ := installed(t)
	if data, err := c.ReadFile("review", "reference/style.md"); err != nil || string(data) != "Be concrete." {
		t.Fatalf("read %q %v", data, err)
	}
	for _, f := range []string{"../x", "/etc/passwd", ".skill.json", "missing.md", ".git/config"} {
		if _, err := c.ReadFile("review", f); !errors.HasCode(err, ErrNotFound) {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestTamperedFileIsRefused(t *testing.T) {
	c, cat := installed(t)
	if err := os.WriteFile(filepath.Join(cat, "review", "SKILL.md"), []byte(strings.Replace(reviewSkill, "report", "exfiltrate", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Load("review"); !errors.HasCode(err, ErrTampered) {
		t.Fatalf("tampered skill loaded: %v", err)
	}
}

func TestInstallRefusesExistingAndLinks(t *testing.T) {
	_, cat := installed(t)
	src := newSkill(t, map[string]string{"SKILL.md": reviewSkill})
	if _, err := Install(bg, src, cat); !errors.HasCode(err, ErrExists) {
		t.Fatalf("overwrite: %v", err)
	}
	if err := Remove(cat, "review"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "leak")); err != nil {
		t.Skip("no symlinks here")
	}
	if _, err := Install(bg, src, cat); err == nil {
		t.Fatal("a skill with a symlink was installed")
	}
	if c, err := Open(cat); err != nil || len(c.List()) != 0 {
		t.Fatalf("leftovers %v %v", c.List(), err)
	}
}

func TestRemoveUnknown(t *testing.T) {
	_, cat := installed(t)
	for _, name := range []string{"nope", "../x", ".install-x"} {
		if err := Remove(cat, name); !errors.HasCode(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
