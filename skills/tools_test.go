// tools_test.go: the skill tools, scripts in the real sandbox
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/sandbox"
)

func call(t *testing.T, tools []agent.Tool, name string, in any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Spec().Name == name {
			out, err := tool.Call(bg, raw)
			return out.Content, err
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

func TestLoadAndReadTools(t *testing.T) {
	c, _ := installed(t)
	tools := Tools(c, Options{})
	if len(tools) != 2 {
		t.Fatalf("without a sandbox there is no run_skill_script: %d tools", len(tools))
	}
	out, err := call(t, tools, "load_skill", map[string]string{"name": "review"})
	if err != nil || !strings.Contains(out, "# Review") || !strings.Contains(out, "scripts/stats.py") {
		t.Fatalf("load %q %v", out, err)
	}
	if _, err := call(t, tools, "load_skill", map[string]string{"name": "nope"}); !strings.Contains(agent.ModelMessage(err), "No skill named nope") {
		t.Fatalf("unknown: %v", err)
	}
}

func testSandbox(t *testing.T) sandbox.Sandbox {
	t.Helper()
	sb, err := sandbox.New()
	if err != nil {
		if os.Getenv("RINGIL_REQUIRE_SANDBOX") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	return sb
}

func TestRunScript(t *testing.T) {
	c, _ := installed(t)
	tools := Tools(c, Options{Sandbox: testSandbox(t)})
	out, err := call(t, tools, "run_skill_script", runIn{Skill: "review", Script: "scripts/stats.py", Args: []string{"-v"}, Stdin: "uno due tre"})
	if err != nil || strings.TrimSpace(out) != "3 words ['-v']" {
		t.Fatalf("run %q %v", out, err)
	}
	for _, bad := range []string{"SKILL.md", "reference/style.md", "scripts/../SKILL.md", "missing.py"} {
		if _, err := call(t, tools, "run_skill_script", runIn{Skill: "review", Script: bad}); err == nil {
			t.Errorf("%s ran", bad)
		}
	}
}

func TestScriptTimeoutAndOutputCap(t *testing.T) {
	src := newSkill(t, map[string]string{
		"SKILL.md":         reviewSkill,
		"scripts/loop.sh":  "while :; do :; done\n",
		"scripts/noise.sh": "head -c 100000 /dev/zero | tr '\\0' x\n",
	})
	cat := t.TempDir()
	if _, err := Install(bg, src, cat); err != nil {
		t.Fatal(err)
	}
	c, err := Open(cat)
	if err != nil {
		t.Fatal(err)
	}
	tools := Tools(c, Options{Sandbox: testSandbox(t), Timeout: time.Second, MaxOutput: 1000})
	start := time.Now()
	if _, err := call(t, tools, "run_skill_script", runIn{Skill: "review", Script: "scripts/loop.sh"}); !errors.HasCode(err, ErrScript) || time.Since(start) > 10*time.Second {
		t.Fatalf("loop: %v after %v", err, time.Since(start))
	}
	out, err := call(t, tools, "run_skill_script", runIn{Skill: "review", Script: "scripts/noise.sh"})
	if err != nil || !strings.HasSuffix(out, "[99000 more bytes of output not shown]") {
		t.Fatalf("noise %d %v", len(out), err)
	}
}

func TestInterpreterChoice(t *testing.T) {
	cases := []struct{ script, data, want string }{
		{"a.py", "", "python3 -B -s"},
		{"tool", "#!/usr/bin/env python3\n", "python3 -B -s"},
		{"tool", "#!/usr/local/bin/python3\n", "python3 -B -s"},
		{"tool", "#!/usr/bin/env -S node --no-warnings\n", "node"},
		{"tool", "#!/bin/zsh -f\n", "zsh -f"},
		{"x.js", "", "node"},
		{"README", "plain text", ""},
		{"tool", "#!/usr/bin/env\n", ""},
	}
	for _, c := range cases {
		argv, ok := interpreter(c.script, []byte(c.data))
		if got := strings.Join(argv, " "); got != c.want || ok != (c.want != "") {
			t.Errorf("%s %q: %q %v", c.script, c.data, got, ok)
		}
	}
}

// Scripts anywhere in the skill, found by #!, with libraries from Env.
func TestShebangScriptAnywhereWithEnv(t *testing.T) {
	lib := t.TempDir()
	if err := os.WriteFile(lib+"/helper.py", []byte("GREETING = 'ciao'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := newSkill(t, map[string]string{
		"SKILL.md":  reviewSkill,
		"bin/greet": "#!/usr/bin/env python3\nimport helper\nprint(helper.GREETING)\n",
	})
	cat := t.TempDir()
	if _, err := Install(bg, src, cat); err != nil {
		t.Fatal(err)
	}
	c, err := Open(cat)
	if err != nil {
		t.Fatal(err)
	}
	tools := Tools(c, Options{Sandbox: testSandbox(t), Mounts: []sandbox.Mount{{Source: lib}}, Env: map[string]string{"PYTHONPATH": lib}})
	out, err := call(t, tools, "run_skill_script", runIn{Skill: "review", Script: "bin/greet"})
	if err != nil || strings.TrimSpace(out) != "ciao" {
		t.Fatalf("greet %q %v", out, err)
	}
}

func TestScriptFilesAreDelivered(t *testing.T) {
	src := newSkill(t, map[string]string{
		"SKILL.md": reviewSkill,
		"scripts/out.sh": "echo nuovo > out.txt; mkdir -p sub .git/x; echo b > sub/b.txt; echo pwn > .git/x/config; echo k > .env; " +
			"touch \"$(printf 'a\\nb')\"; ln -s /etc/passwd link; mkdir locked; echo l > locked/f; chmod 000 locked; echo fatto; exit ${1:-0}\n",
	})
	cat := t.TempDir()
	if _, err := Install(bg, src, cat); err != nil {
		t.Fatal(err)
	}
	c, err := Open(cat)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	tools := Tools(c, Options{Sandbox: testSandbox(t), Output: dest, Runtimes: map[string]string{"bash": "/usr/bin/bash"}})
	out, err := call(t, tools, "run_skill_script", runIn{Skill: "review", Script: "scripts/out.sh"})
	if err != nil || !strings.Contains(out, `"out.txt" "sub/b.txt"`) || !strings.Contains(out, "entries not saved") {
		t.Fatalf("run %q %v", out, err)
	}
	checkDelivered(t, dest)
	// A failing script still hands over what it made, in a new folder.
	_, err = call(t, tools, "run_skill_script", runIn{Skill: "review", Script: "scripts/out.sh", Args: []string{"3"}})
	if msg := agent.ModelMessage(err); !strings.Contains(msg, "Files saved in") {
		t.Fatalf("failed run: %q", msg)
	}
}

// Audit 18/11: files went straight into the start directory, .git included.
func checkDelivered(t *testing.T, dest string) {
	t.Helper()
	runs, err := os.ReadDir(dest)
	if err != nil || len(runs) != 1 || !strings.HasPrefix(runs[0].Name(), "review-") {
		t.Fatalf("output folder %v %v", runs, err)
	}
	var got []string
	walkErr := filepath.WalkDir(filepath.Join(dest, runs[0].Name()), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			got = append(got, d.Name())
		}
		return err
	})
	if walkErr != nil || strings.Join(got, ",") != "f,out.txt,b.txt" {
		t.Fatalf("delivered %v %v", got, walkErr)
	}
}

// Audit 18/11: a locked /work, empty folders, a full output folder.
func TestDeliveryEdgeCases(t *testing.T) {
	src := newSkill(t, map[string]string{
		"SKILL.md":        reviewSkill,
		"scripts/lock.sh": "echo 1 > f; chmod 000 f; chmod 000 /work\n",
		"scripts/dots.sh": "echo k > .env\n",
		"scripts/big.sh":  "head -c 3000 /dev/zero > big\n",
	})
	cat := t.TempDir()
	if _, err := Install(bg, src, cat); err != nil {
		t.Fatal(err)
	}
	c, err := Open(cat)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	tools := Tools(c, Options{Sandbox: testSandbox(t), Output: dest, OutputQuota: 4000})
	if out, err := call(t, tools, "run_skill_script", runIn{Skill: "review", Script: "scripts/lock.sh"}); err != nil || !strings.Contains(out, `"f"`) {
		t.Fatalf("locked work dir: %q %v", out, err)
	}
	if out, err := call(t, tools, "run_skill_script", runIn{Skill: "review", Script: "scripts/dots.sh"}); err != nil || strings.Contains(out, "Files saved") {
		t.Fatalf("hidden only: %q %v", out, err)
	}
	if runs, _ := os.ReadDir(dest); len(runs) != 1 {
		t.Fatalf("empty folder left: %v", runs)
	}
	checkQuota(t, tools)
}

// The quota leaves room for one 3000-byte file, not two.
func checkQuota(t *testing.T, tools []agent.Tool) {
	t.Helper()
	if _, err := call(t, tools, "run_skill_script", runIn{Skill: "review", Script: "scripts/big.sh"}); err != nil {
		t.Fatalf("first big: %v", err)
	}
	out, err := call(t, tools, "run_skill_script", runIn{Skill: "review", Script: "scripts/big.sh"})
	if err != nil || strings.Contains(out, "Files saved") || !strings.Contains(out, "entries not saved") {
		t.Fatalf("quota not enforced: %q %v", out, err)
	}
}
