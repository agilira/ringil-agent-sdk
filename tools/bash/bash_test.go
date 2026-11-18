// bash_test.go: the tool, run for real
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/sandbox"
)

func boxOrSkip(t *testing.T) sandbox.Sandbox {
	t.Helper()
	s, err := sandbox.New()
	if err != nil {
		if os.Getenv("RINGIL_REQUIRE_SANDBOX") != "" && runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	return s
}

func newBash(t *testing.T, cfg Config) agent.Tool {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	tl, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func call(t *testing.T, tl agent.Tool, in map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	out, err := tl.Call(context.Background(), raw)
	return out.Content, err
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSandboxedCommandRunsWithoutAsking(t *testing.T) {
	box := boxOrSkip(t)
	dir := t.TempDir()
	write(t, dir, map[string]string{"a.txt": "hello", ".env": "SECRET=1", "CLAUDE.md": "rules", ".git/hooks/pre-commit": "x", ".git/config": "[core]"})
	var asked atomic.Int32
	tl := newBash(t, Config{Dir: dir, Sandbox: box, Read: []string{}, Confirm: func(context.Context, Request) (bool, error) {
		asked.Add(1)
		return false, nil
	}})
	out, err := call(t, tl, map[string]any{"command": `pwd -P; cat a.txt; echo; cat .env 2>/dev/null; echo new > b.txt && echo wrote;
(echo x >> CLAUDE.md) 2>/dev/null || echo denied; (echo x > .git/hooks/post-commit) 2>/dev/null || echo denied;
(echo x > .git/config) 2>/dev/null || echo denied; echo obj > .git/objects-ok && echo git-writable;
touch /etc/x 2>/dev/null || echo etc-ro; ls ~ 2>/dev/null | wc -l | tr -d ' '`})
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir) // macOS: /var is /private/var
	want := real + "\nhello\nwrote\ndenied\ndenied\ndenied\ngit-writable\netc-ro\n0\n"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	if asked.Load() != 0 {
		t.Fatal("a safe sandboxed command asked")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(b) != "new\n" {
		t.Fatal("the working directory was not written")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(b) != "rules" {
		t.Fatal("a protected file changed")
	}
}

func TestExitStatusTimeoutAndHints(t *testing.T) {
	box := boxOrSkip(t)
	tl := newBash(t, Config{Sandbox: box, Read: []string{}})
	out, _ := call(t, tl, map[string]any{"command": "echo partial; exit 3"})
	if out != "Exit status 3.\npartial\n" {
		t.Fatalf("%q", out)
	}
	out, _ = call(t, tl, map[string]any{"command": "sleep 30", "timeout": 1})
	if !strings.HasPrefix(out, "Stopped after 1s") {
		t.Fatalf("%q", out)
	}
	out, _ = call(t, tl, map[string]any{"command": "touch /usr/x"})
	if !strings.Contains(out, "[Sandbox: a protected file") {
		t.Fatalf("no hint: %q", out)
	}
	out, _ = call(t, tl, map[string]any{"command": "true"})
	if out != "(no output)" {
		t.Fatalf("%q", out)
	}
}

// confirmer records the requests and answers with *answer.
func confirmer(got *[]Request, answer *bool) func(context.Context, Request) (bool, error) {
	return func(_ context.Context, r Request) (bool, error) {
		*got = append(*got, r)
		return *answer, nil
	}
}

func TestDangerousNeedsConfirm(t *testing.T) {
	box := boxOrSkip(t)
	dir := t.TempDir()
	write(t, dir, map[string]string{"build/x": "1"})
	var got []Request
	answer := false
	tl := newBash(t, Config{Dir: dir, Sandbox: box, Read: []string{}, Confirm: confirmer(&got, &answer)})
	if _, err := call(t, tl, map[string]any{"command": "rm -rf build"}); !errors.HasCode(err, ErrRefused) {
		t.Fatalf("refused dangerous command ran: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build/x")); err != nil {
		t.Fatal("refused command deleted the tree")
	}
	answer = true
	if _, err := call(t, tl, map[string]any{"command": "rm -rf build"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build")); err == nil {
		t.Fatal("allowed command did not run")
	}
	if len(got) != 2 || got[0].Reason != Destructive || got[0].Why[0] != "rm -r deletes whole directories" {
		t.Fatalf("requests %+v", got)
	}
}

func TestUnsandboxedNeedsConfirm(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	var got []Request
	answer := true
	tl := newBash(t, Config{Sandbox: noBox{}, Read: []string{}, Confirm: confirmer(&got, &answer)})
	out, err := call(t, tl, map[string]any{"command": "echo outside", "unsandboxed": true, "reason": "needs ssh"})
	if err != nil || out != "outside\n" {
		t.Fatalf("%q %v", out, err)
	}
	if len(got) != 1 || got[0].Reason != Unsandboxed || got[0].Explanation != "needs ssh" {
		t.Fatalf("requests %+v", got)
	}
}

func TestNoConfirmRefuses(t *testing.T) {
	tl := newBash(t, Config{Sandbox: noBox{}, Read: []string{}})
	_, err := call(t, tl, map[string]any{"command": "echo hi", "unsandboxed": true})
	if !errors.HasCode(err, ErrRefused) {
		t.Fatalf("%v", err)
	}
}

type noBox struct{}

func (noBox) Start(context.Context, sandbox.Spec) (sandbox.Process, error) {
	return nil, errors.New(sandbox.ErrStart, "not here")
}

func TestHostEnvDropsCredentials(t *testing.T) {
	env := hostEnv([]string{"PATH=/bin", "NVIDIA_NIM_API_KEY=x", "GITHUB_TOKEN=y", "SSH_AUTH_SOCK=/s", "AWS_SECRET_ACCESS_KEY=z", "HOME=/h"})
	joined := strings.Join(env, " ")
	for _, gone := range []string{"NIM", "GITHUB_TOKEN", "AWS"} {
		if strings.Contains(joined, gone) {
			t.Errorf("%s passed: %s", gone, joined)
		}
	}
	for _, kept := range []string{"PATH=/bin", "SSH_AUTH_SOCK=/s", "HOME=/h"} {
		if !strings.Contains(joined, kept) {
			t.Errorf("%s dropped", kept)
		}
	}
}

func TestCaptureKeepsStartAndEnd(t *testing.T) {
	c := &capture{max: 10}
	for _, s := range []string{"abcde", "fghij", "klmno", "pqrst"} {
		_, _ = c.Write([]byte(s))
	}
	if got := c.String(); got != "abcde\n[... 10 bytes of output left out ...]\npqrst" {
		t.Fatalf("%q", got)
	}
}

func TestReadPathsAndHome(t *testing.T) {
	box := boxOrSkip(t)
	home := t.TempDir()
	write(t, home, map[string]string{".gitconfig": "[user]\n\tname = T\n", "sdk/go/bin/tool": "#!/bin/sh\necho tool-ran\n", "sdk/go/.env": "S=1", "other/x": "no"})
	if err := os.Chmod(filepath.Join(home, "sdk/go/bin/tool"), 0o700); err != nil { // #nosec G302 -- a test executable
		t.Fatal(err)
	}
	work := filepath.Join(home, "proj")
	write(t, work, map[string]string{"m.txt": "m"})
	t.Setenv("PATH", filepath.Join(home, "sdk/go/bin")+":/usr/bin:/bin:/opt/missing")
	tl := newBash(t, Config{Dir: work, Sandbox: box, Home: home,
		Read: []string{filepath.Join(home, ".gitconfig"), filepath.Join(home, "sdk/go")}})
	out, err := call(t, tl, map[string]any{"command": `echo "$HOME"; head -1 ~/.gitconfig; tool; cat ~/sdk/go/.env 2>/dev/null; ls ~/other 2>&1 | grep -cE "No such|not permitted"; echo "$PATH"`})
	if err != nil {
		t.Fatal(err)
	}
	want := home + "\n[user]\ntool-ran\n1\n" + filepath.Join(home, "sdk/go/bin") + ":/usr/bin:/bin\n"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}
