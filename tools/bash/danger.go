// danger.go: commands the user confirms even in the sandbox
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Dangerous lists why command needs the user's yes even in the sandbox:
// it destroys work the sandbox lets it reach (a tree, uncommitted changes,
// history) or reaches outside (a push). Empty means none of those.
//
// It reads the command as the shell would, through pipes, lists,
// subshells, functions and bash -c strings, but it is a safety net for a
// model's mistakes, not a boundary: the sandbox is the boundary. A command
// it cannot read (a name built at run time, eval, a syntax error) is
// dangerous by default.
func Dangerous(command string) []string {
	return dangerous(command, 0)
}

const maxNesting = 4

func dangerous(command string, depth int) []string {
	if depth > maxNesting {
		return []string{"commands nested too deeply to check"}
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return []string{"the command cannot be parsed"}
	}
	var why []string
	syntax.Walk(file, func(n syntax.Node) bool {
		if call, ok := n.(*syntax.CallExpr); ok && len(call.Args) > 0 {
			why = append(why, callDanger(words(call.Args), depth)...)
		}
		return true
	})
	return unique(why)
}

// dynamic marks a word whose value is known only at run time.
const dynamic = "\x00"

// words returns the call's words as the shell would see them, with quotes
// removed; a word holding an expansion is dynamic.
func words(args []*syntax.Word) []string {
	out := make([]string, len(args))
	for i, w := range args {
		out[i] = literal(w)
	}
	return out
}

func literal(w *syntax.Word) string {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(unescape(p.Value))
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return dynamic
				}
				b.WriteString(lit.Value)
			}
		default:
			return dynamic
		}
	}
	return b.String()
}

// unescape removes the backslashes the shell removes from an unquoted
// word: \rm is rm.
func unescape(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == '\n' {
				continue // a line continuation
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// wrappers run the command that follows them; their own options are
// skipped.
var wrappers = map[string]bool{"command": true, "builtin": true, "exec": true, "nice": true, "nohup": true,
	"time": true, "timeout": true, "env": true, "stdbuf": true, "xargs": true, "ionice": true, "setsid": true}

var shells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true}

func callDanger(args []string, depth int) []string {
	args = unwrap(args)
	if len(args) == 0 {
		return nil
	}
	name := args[0]
	if name == dynamic || strings.ContainsAny(name, "{*?[") {
		return []string{"the command's name is built at run time"}
	}
	base := path.Base(name)
	switch {
	case shells[base]:
		return shellDanger(args[1:], depth)
	case base == "find": // its -exec runs a command: checked like this one
		return findDanger(args[1:])
	}
	if check, ok := commandChecks[base]; ok {
		return check(base, args[1:])
	}
	return nil
}

func always(why string) func(string, []string) []string {
	return func(base string, _ []string) []string { return []string{base + " " + why} }
}

// commandChecks says, per command, why its arguments make it dangerous.
var commandChecks = map[string]func(base string, args []string) []string{
	"eval":   always("runs text the check cannot see"),
	"source": always("runs a file the check cannot see"),
	".":      always("runs a file the check cannot see"),
	"sudo":   always("runs as another user"),
	"doas":   always("runs as another user"),
	"su":     always("runs as another user"),
	"pkexec": always("runs as another user"),
	"shred":  always("destroys files"),
	"rm":     func(_ string, a []string) []string { return rmDanger(a) },
	"git":    func(_ string, a []string) []string { return gitDanger(a) },
}

// unwrap drops leading wrappers (env, timeout, xargs...) and their options,
// leaving the command they run.
func unwrap(args []string) []string {
	for len(args) > 0 && wrappers[path.Base(args[0])] {
		args = args[1:]
		for len(args) > 0 && (strings.HasPrefix(args[0], "-") || strings.Contains(args[0], "=") || isDuration(args[0])) {
			args = args[1:]
		}
	}
	return args
}

// isDuration recognises timeout's first operand (10, 5s, 1.5m).
func isDuration(s string) bool {
	s = strings.TrimRight(s, "smhd")
	return s != "" && strings.Trim(s, "0123456789.") == ""
}

// shellDanger checks the script of sh -c SCRIPT; a shell reading a file or
// standard input runs what the check cannot see.
func shellDanger(args []string, depth int) []string {
	for i, a := range args {
		if a == dynamic {
			break
		}
		if !strings.HasPrefix(a, "-") {
			return []string{"a shell runs a script the check cannot see"}
		}
		if a == "-c" || !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
			if i+1 < len(args) && args[i+1] != dynamic {
				return dangerous(args[i+1], depth+1)
			}
			break
		}
	}
	return []string{"a shell runs commands the check cannot see"}
}

func rmDanger(args []string) []string {
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--recursive" || a == dynamic || shortFlag(a, 'r') || shortFlag(a, 'R') {
			return []string{"rm -r deletes whole directories"}
		}
	}
	return nil
}

func findDanger(args []string) []string {
	for i, a := range args {
		switch {
		case a == "-delete":
			return []string{"find -delete deletes files"}
		case (a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir") && i+1 < len(args):
			if why := callDanger(args[i+1:], maxNesting); len(why) > 0 {
				return why
			}
		}
	}
	return nil
}

// shortFlag reports whether a is a cluster of short options holding c
// (-rf, -fr, -r).
func shortFlag(a string, c byte) bool {
	return len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.IndexByte(a[1:], c) >= 0
}

// gitGlobalWithValue are git's options before the subcommand that take
// the next word as their value.
var gitGlobalWithValue = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true, "--config-env": true}

func gitDanger(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if gitGlobalWithValue[args[0]] {
			args = args[1:]
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return nil
	}
	sub, rest := args[0], args[1:]
	if sub == dynamic {
		return []string{"the git command is built at run time"}
	}
	if check, ok := gitChecks[sub]; ok {
		if why := check(rest); why != "" {
			return []string{why}
		}
	}
	return nil
}

func has(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n || strings.HasPrefix(n, "--") && strings.HasPrefix(a, n+"=") {
				return true
			}
		}
	}
	return false
}

func hasShort(args []string, c byte) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if shortFlag(a, c) {
			return true
		}
	}
	return false
}

// gitChecks says, per subcommand, why its arguments make it dangerous.
var gitChecks = map[string]func([]string) string{
	"push": func([]string) string { return "git push publishes to a remote" },
	"reset": func(a []string) string {
		return when(has(a, "--hard", "--merge", "--keep"), "git reset --hard discards uncommitted changes")
	},
	"clean": func(a []string) string {
		return when(has(a, "--force") || hasShort(a, 'f'), "git clean -f deletes untracked files")
	},
	"checkout": func(a []string) string {
		return when(has(a, "--", ".", "--force", "--discard-changes", "--ours", "--theirs") || hasShort(a, 'f'),
			"git checkout over files discards their changes")
	},
	"restore": func(a []string) string {
		staged := has(a, "--staged") || hasShort(a, 'S')
		worktree := has(a, "--worktree") || hasShort(a, 'W')
		return when(!staged || worktree, "git restore discards changes in the working tree")
	},
	"switch": func(a []string) string {
		return when(has(a, "--discard-changes", "--force") || hasShort(a, 'f'), "git switch --discard-changes discards changes")
	},
	"branch": func(a []string) string {
		force := has(a, "--force") || hasShort(a, 'f')
		del := has(a, "--delete") || hasShort(a, 'd')
		return when(hasShort(a, 'D') || hasShort(a, 'M') || del && force, "git branch -D deletes unmerged work")
	},
	"stash": func(a []string) string {
		return when(len(a) > 0 && (a[0] == "drop" || a[0] == "clear"), "git stash drop deletes stashed work")
	},
	"rebase":        func([]string) string { return "git rebase rewrites history" },
	"filter-branch": func([]string) string { return "git filter-branch rewrites history" },
	"filter-repo":   func([]string) string { return "git filter-repo rewrites history" },
	"reflog": func(a []string) string {
		return when(len(a) > 0 && (a[0] == "expire" || a[0] == "delete"), "git reflog expire drops recovery points")
	},
	"update-ref": func(a []string) string {
		return when(has(a, "-d", "--delete"), "git update-ref -d deletes a reference")
	},
	"gc": func(a []string) string {
		for _, x := range a {
			if strings.HasPrefix(x, "--prune") {
				return "git gc --prune drops unreachable work"
			}
		}
		return ""
	},
	"commit": func(a []string) string {
		return when(has(a, "--amend"), "git commit --amend rewrites the last commit")
	},
}

func when(cond bool, why string) string {
	if cond {
		return why
	}
	return ""
}

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
