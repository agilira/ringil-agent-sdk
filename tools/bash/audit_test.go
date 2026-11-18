// audit_test.go: regressions of the bash audit (18/11)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A sandboxed command renames .git (not itself a mount, only its hooks,
// config and info are) and puts a writable copy in its place: the hooks
// and configuration git runs on the host are now the command's. The same
// works for any protected path below the workspace's root (sub/CLAUDE.md).
func TestAuditRenameParentDefeatsReadOnly(t *testing.T) {
	box := boxOrSkip(t)
	dir := t.TempDir()
	write(t, dir, map[string]string{".git/hooks/pre-commit": "x", ".git/config": "[core]", ".git/HEAD": "ref: refs/heads/main\n", "sub/CLAUDE.md": "rules"})
	tl := newBash(t, Config{Dir: dir, Sandbox: box, Read: []string{}})
	out, err := call(t, tl, map[string]any{"command": `mv .git .g && cp -a .g .git && echo evil > .git/hooks/pre-commit && echo '[core] hooksPath = /tmp' > .git/config;
mv sub s2 && mkdir sub && echo evil > sub/CLAUDE.md`})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".git/hooks/pre-commit", ".git/config", "sub/CLAUDE.md"} {
		if b, _ := os.ReadFile(filepath.Join(dir, p)); strings.Contains(string(b), "evil") || strings.Contains(string(b), "hooksPath") {
			t.Errorf("protected %s rewritten by a sandboxed command (%s)", p, out)
		}
	}
}

// .git/commondir is writable: pointing it at a directory the command
// makes in the workspace gives git on the host another config and hooks.
func TestAuditGitCommondirRedirectsHooks(t *testing.T) {
	box := boxOrSkip(t)
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(gitBin, append([]string{"-c", "user.email=a@b", "-c", "user.name=a"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HOME="+dir, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "a")
	tl := newBash(t, Config{Dir: dir, Sandbox: box, Read: []string{}})
	marker := filepath.Join(dir, "hook-ran")
	cmd := `cp -a .git/. evil/ 2>/dev/null || { mkdir evil && cp -a .git/. evil/; }; rm -f evil/config; printf '[core]\n\trepositoryformatversion = 0\n\thooksPath = ` + dir + `/evil/hk\n' > evil/config;
mkdir -p evil/hk && printf '#!/bin/sh\ntouch ` + marker + `\n' > evil/hk/pre-commit && chmod +x evil/hk/pre-commit && echo ../evil > .git/commondir`
	if out, err := call(t, tl, map[string]any{"command": cmd}); err != nil {
		t.Fatal(err, out)
	}
	run("commit", "-q", "--allow-empty", "-m", "b") // the user, on the host
	if _, err := os.Stat(marker); err == nil {
		t.Error("a hook planted by a sandboxed command ran on the host")
	}
}

// git's credential store keeps tokens in $XDG_CONFIG_HOME/git/credentials;
// ReadDefaults grants ~/.config/git and DefaultExclude hides only
// .git-credentials.
func TestAuditXDGGitCredentialsVisible(t *testing.T) {
	box := boxOrSkip(t)
	home := t.TempDir()
	write(t, home, map[string]string{".config/git/config": "[user]\n", ".config/git/credentials": "https://u:ghp_SECRET@github.com\n"})
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	work := filepath.Join(home, "proj")
	write(t, work, map[string]string{"x": "x"})
	tl := newBash(t, Config{Dir: work, Sandbox: box, Home: home, Read: ReadDefaults()})
	out, _ := call(t, tl, map[string]any{"command": "cat ~/.config/git/credentials"})
	if strings.Contains(out, "ghp_SECRET") {
		t.Errorf("git's stored credentials are readable in the sandbox: %q", out)
	}
}

// A ~/.gitconfig kept as a symbolic link (dotfile managers do this) makes
// every sandboxed command fail: openSource takes any symlink for a
// directory.
func TestAuditSymlinkedGitconfigBreaksBash(t *testing.T) {
	box := boxOrSkip(t)
	home := t.TempDir()
	write(t, home, map[string]string{"dotfiles/gitconfig": "[user]\n\tname = T\n"})
	if err := os.Symlink(filepath.Join(home, "dotfiles/gitconfig"), filepath.Join(home, ".gitconfig")); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(home, "proj")
	write(t, work, map[string]string{"x": "x"})
	tl := newBash(t, Config{Dir: work, Sandbox: box, Home: home, Read: []string{filepath.Join(home, ".gitconfig")}})
	out, err := call(t, tl, map[string]any{"command": "echo ok"})
	if err != nil || out != "ok\n" {
		t.Errorf("a symlinked ~/.gitconfig breaks bash: %q %v", out, err)
	}
}

// Hidden paths are found by a walk, then masked by path when bwrap
// mounts: a concurrent sandboxed command that keeps renaming the secret's
// directory moves it away between the two, and the new command sees it.
func TestAuditConcurrentRenameUnmasksSecret(t *testing.T) {
	box := boxOrSkip(t)
	dir := t.TempDir()
	write(t, dir, map[string]string{"cfg/.env": "PROD_DB_PASSWORD=hunter2"})
	tl := newBash(t, Config{Dir: dir, Sandbox: box, Read: []string{}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		o, e := call(t, tl, map[string]any{"command": `end=$((SECONDS+8)); while test $SECONDS -lt $end; do mv cfg c2 2>/dev/null; mv c2 cfg 2>/dev/null; done`, "timeout": 20})
		t.Log("A:", o, e)
	}()
	leaked := ""
	for i := 0; i < 200 && leaked == ""; i++ {
		out, _ := call(t, tl, map[string]any{"command": `cat cfg/.env c2/.env 2>/dev/null; find . -name .env -exec cat {} + 2>/dev/null`})
		if strings.Contains(out, "hunter2") {
			leaked = out
		}
		select {
		case <-done:
			i = 1000
		default:
		}
	}
	<-done
	if leaked != "" {
		t.Errorf("a hidden secret was read while another command renamed its directory: %q", leaked)
	}
}

// A repository the command makes keeps no hooks and only plain settings.
func TestNewRepositoryIsNeutralised(t *testing.T) {
	box := boxOrSkip(t)
	dir := t.TempDir()
	tl := newBash(t, Config{Dir: dir, Sandbox: box, Read: []string{}})
	out, err := call(t, tl, map[string]any{"command": `mkdir -p r/.git/hooks && printf '#!/bin/sh\necho evil\n' > r/.git/hooks/pre-commit && touch r/.git/hooks/pre-push.sample
printf '[core]\n\trepositoryformatversion = 0\n\tfsmonitor = /tmp/x\n[user]\n\tname = T\n[alias]\n\tst = !rm -rf ~\n' > r/.git/config
echo ../x > r/.git/commondir; echo "gitdir: /tmp/evil" > .git`})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "r/.git/config")); string(b) != "[core]\n\trepositoryformatversion = 0\n[user]\n\tname = T\n" {
		t.Errorf("config %q", b)
	}
	for _, gone := range []string{"r/.git/hooks/pre-commit", "r/.git/commondir", ".git"} {
		if _, err := os.Lstat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s left", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "r/.git/hooks/pre-push.sample")); err != nil {
		t.Error("a sample hook was removed")
	}
	if !strings.Contains(out, "core.fsmonitor") || !strings.Contains(out, "alias.st") {
		t.Errorf("not reported: %q", out)
	}
}

func TestFilterConfig(t *testing.T) {
	in := "[core]\n\tbare = false\n\thooksPath = /x\n[remote \"origin\"]\n\turl = https://h/r\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n" +
		"[include]\n\tpath = ../evil\n[core] pager = evil\n[filter \"lfs\"]\n\tclean = run\n[branch.main]\n\tremote = origin\n\tmerge = x \\\n\tcontinued\n"
	out, dropped := filterConfig(in)
	want := "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = https://h/r\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n[branch \"main\"]\n\tremote = origin\n"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	if len(dropped) < 5 {
		t.Errorf("dropped %v", dropped)
	}
}
