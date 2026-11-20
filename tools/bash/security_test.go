// security_test.go: fuzz and security tests for dangerous bash detection and git config sanitization
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"strings"
	"testing"
)

func FuzzDangerous(f *testing.F) {
	seeds := []string{
		"ls -la",
		"go test ./... && git status",
		"rm -rf build",
		"rm -r -- build",
		`env -S 'rm -rf x'`,
		`env --split-string="git push"`,
		"timeout -s KILL 60 git push origin main",
		`find . -name '*.o' | xargs -d '\n' rm -rf`,
		"find . -execdir rm -rf {} +",
		`bash -c "bash -c 'git reset --hard'"`,
		`sh -c "sh -c 'sh -c \"sh -c '\''sh -c true'\''\"'"`,
		"git -c alias.p=push p origin main",
		"git --config-env=alias.x=VAR x",
		`$'\x72m' -rf build`,
		"r{m,} -rf build",
		"echo 'unterminated",
		"eval \"$CMD\"",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		if len(cmd) > 4096 {
			return
		}
		why := Dangerous(cmd)
		checkUniqueNonEmpty(t, cmd, why)
	})
}

func checkUniqueNonEmpty(t *testing.T, input string, items []string) {
	t.Helper()
	seen := make(map[string]bool, len(items))
	for _, w := range items {
		if w == "" {
			t.Fatalf("empty reason returned for %q", input)
		}
		if seen[w] {
			t.Fatalf("duplicate reason %q for %q", w, input)
		}
		seen[w] = true
	}
}

func FuzzFilterConfig(f *testing.F) {
	seeds := []string{
		"[core]\n\tbare = false\n",
		"[core]\n\thooksPath = /evil\n\tfsmonitor = /tmp/x\n",
		"[core]\n\tsshCommand = evil\n\tpager = evil\n",
		"[include]\n\tpath = /etc/passwd\n",
		"[includeIf \"gitdir:/\"]\n\tpath = /tmp/x\n",
		"[alias]\n\tst = !rm -rf /\n",
		"[filter \"lfs\"]\n\tclean = evil\n\tsmudge = evil\n",
		"[remote \"origin\"]\n\turl = https://example.com/r.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n",
		"[remote \"a\\\"b\"]\n\turl = https://example.com/r.git\n",
		"[branch.main]\n\tremote = origin\n\tmerge = x \\\n\tcontinued\n",
		"[unclosed\n\tbare = false\n",
		"[core] hooksPath = /tmp\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 8192 {
			return
		}
		kept, _ := filterConfig(text)
		kept2, dropped2 := filterConfig(kept)
		if len(dropped2) != 0 || kept2 != kept {
			t.Fatalf("filterConfig not idempotent: dropped2=%v\nkept1:\n%s\nkept2:\n%s", dropped2, kept, kept2)
		}
		assertSafeKeptConfig(t, kept)
	})
}

var forbiddenConfigFragments = []string{
	"hookspath", "fsmonitor", "sshcommand", "askpass", "gitproxy",
	"uploadpack", "receivepack", "textconv", "clean", "smudge",
	"[include", "[alias", "[credential", "[filter",
}

func assertSafeKeptConfig(t *testing.T, kept string) {
	t.Helper()
	for _, line := range strings.Split(kept, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), `\`) {
			t.Fatalf("kept line with trailing continuation: %q", line)
		}
	}
	lower := strings.ToLower(kept)
	for _, frag := range forbiddenConfigFragments {
		if strings.Contains(lower, frag) {
			t.Fatalf("forbidden fragment %q survived in:\n%s", frag, kept)
		}
	}
}

func TestSecurityDangerousEvasionAttempts(t *testing.T) {
	mustFlag := []string{
		`sh -c "sh -c 'sh -c \"sh -c '\''sh -c true'\''\"'"`,
		"rm $FLAGS dir",
		"rm -r -- dir",
		`env --split-string="rm -rf /"`,
		"env -Srm -rf /",
		"env --chdir=/tmp git push",
		"exec -a ls rm -rf x",
		"find . -execdir rm -rf {} +",
		`find . -ok rm -rf {} \;`,
		`find . -okdir sh -c 'git push' \;`,
		"git --config-env=alias.x=VAR x",
		"git --config-env alias.x=VAR x",
		"git -c ALIAS.p=push p",
		"git -c $CFG status",
		"git worktree remove -f ../wt",
		"git branch -M main",
		"git branch -df feat",
		"git update-ref -d refs/heads/x",
		"git stash clear",
		"git reflog delete HEAD@{0}",
		"git filter-branch",
		"git filter-repo",
		"git restore --staged --worktree a.go",
		"git switch --discard-changes main",
		"doas ls",
		"su root",
		"pkexec id",
		"source script.sh",
		". script.sh",
		"ksh -c 'rm -rf x'",
		"zsh -c 'git push'",
		"dash -c 'rm -rf x'",
		"/bin/r? -rf x",
		"/bin/r* -rf x",
		"r[m] -rf x",
		"$(echo rm) -rf x",
		"`echo rm` -rf x",
		"r''m -rf x",
		`r""m -rf x`,
		`"rm" -rf x`,
	}
	assertDangerousAll(t, mustFlag, true)

	mustAllow := []string{
		"rm -- -rf",
		"git restore --staged a.go",
		"git branch -d merged",
		"git worktree list",
	}
	assertDangerousAll(t, mustAllow, false)
}

// A valued flag at the end, its value missing, runs nothing: no panic,
// nothing to report. With the value there, what follows is still judged.
func TestSecurityDangerousTrailingValuedFlags(t *testing.T) {
	for _, tc := range []struct{ bare, full, want string }{
		{"xargs -a", "xargs -a list rm -rf /", "rm -r"},
		{"env -u", "env -u HOME rm -rf /", "rm -r"},
		{"timeout -s", "timeout -s KILL 5 rm -rf /", "rm -r"},
		{"nice -n", "nice -n 5 rm -rf /", "rm -r"},
		{"exec -a", "exec -a name rm -rf /", "rm -r"},
		{"watch -n", "watch -n 1 rm -rf /", "rm -r"},
		{"watch --interval", "watch --interval 1 rm -rf /", "rm -r"},
		{"git -C", "git -C repo push --force", "git push"},
		{"git -c", "git -c a=b push --force", "git push"},
		{"git --git-dir", "git --git-dir x push --force", "git push"},
	} {
		if got := Dangerous(tc.bare); len(got) != 0 {
			t.Errorf("Dangerous(%q) = %q, want nothing", tc.bare, got)
		}
		got := Dangerous(tc.full)
		if len(got) == 0 || !strings.HasPrefix(got[0], tc.want) {
			t.Errorf("Dangerous(%q) = %q, want %q", tc.full, got, tc.want)
		}
	}
}

func assertDangerousAll(t *testing.T, cmds []string, wantDangerous bool) {
	t.Helper()
	for _, cmd := range cmds {
		why := Dangerous(cmd)
		if (len(why) > 0) != wantDangerous {
			t.Errorf("%q: dangerous=%v reasons=%v, want %v", cmd, len(why) > 0, why, wantDangerous)
		}
	}
}

func TestSecurityFilterConfigBlocksExecutionDirectives(t *testing.T) {
	cases := map[string]string{
		"hooksPath":       "[core]\n\thooksPath = /tmp/evil\n",
		"upperHooksPath":  "[CORE]\n\tHOOKSPATH = /tmp/evil\n",
		"fsmonitor":       "[core]\n\tfsmonitor = /tmp/evil\n",
		"sshCommand":      "[core]\n\tsshCommand = id >/tmp/pwn\n",
		"pager":           "[core]\n\tpager = sh -c id\n",
		"editor":          "[core]\n\teditor = sh\n",
		"askPass":         "[core]\n\taskPass = /tmp/evil\n",
		"gitProxy":        "[core]\n\tgitProxy = /tmp/evil\n",
		"include":         "[include]\n\tpath = /etc/passwd\n",
		"includeIf":       "[includeIf \"gitdir:/\"]\n\tpath = /tmp/evil\n",
		"alias":           "[alias]\n\tst = !id\n",
		"credential":      "[credential]\n\thelper = !id\n",
		"filterClean":     "[filter \"lfs\"]\n\tclean = evil\n\tsmudge = evil\n",
		"diffTextconv":    "[diff \"x\"]\n\ttextconv = evil\n",
		"remoteUpload":    "[remote \"origin\"]\n\tuploadpack = evil\n\treceivepack = evil\n\tproxy = http://evil\n",
		"inlineCore":      "[core] hooksPath = /tmp/evil\n",
		"malformedHeader": "[core\n\tbare = false\n",
		"continuation":    "[core]\n\tbare = false \\\n\thooksPath = /tmp/evil\n",
	}
	for name, cfg := range cases {
		kept, dropped := filterConfig(cfg)
		if len(dropped) == 0 {
			t.Errorf("%s: expected dropped directives, got none (kept=%q)", name, kept)
		}
		if strings.TrimSpace(kept) != "" {
			t.Errorf("%s: expected empty kept config, got %q", name, kept)
		}
	}
}
