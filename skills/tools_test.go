// tools_test.go: the skill tools, scripts in the real sandbox
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"encoding/json"
	"os"
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
