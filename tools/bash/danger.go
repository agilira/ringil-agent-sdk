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
			if p.Dollar {
				return dynamic // $'\x72m': escapes the check does not decode
			}
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

// wrappers run the command that follows their options; the value lists
// the options that take the next word as their value.
var wrappers = map[string][]string{
	"command": nil, "builtin": nil, "nohup": nil, "setsid": nil, "busybox": nil,
	"exec":    {"-a"},
	"nice":    {"-n", "--adjustment"},
	"time":    {"-f", "-o", "--format", "--output"},
	"timeout": {"-s", "-k", "--signal", "--kill-after"},
	"env":     {"-u", "-C", "--unset", "--chdir"},
	"stdbuf":  {"-i", "-o", "-e", "--input", "--output", "--error"},
	"ionice":  {"-c", "-n", "-p", "-P", "-u", "--class", "--classdata", "--pid", "--pgid", "--uid"},
	"xargs": {"-a", "-d", "-E", "-I", "-L", "-n", "-P", "-s", "--arg-file", "--delimiter", "--eof", "--replace",
		"--max-lines", "--max-args", "--max-procs", "--max-chars", "--process-slot-var"},
}

var shells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true}

func callDanger(args []string, depth int) []string {
	args = unwrap(args)
	if len(args) == 0 {
		return nil
	}
	name := args[0]
	if builtAtRunTime(name) {
		return []string{"the command's name is built at run time"}
	}
	base := path.Base(name)
	switch {
	case shells[base]:
		return shellDanger(args[1:], depth)
	case base == "find": // its -exec runs a command: checked like this one
		return findDanger(args[1:])
	case base == "trap" || base == "watch": // they run their text later
		return scriptDanger(base, args[1:], depth)
	case base == "git":
		return gitDanger(args[1:], depth)
	}
	if check, ok := commandChecks[base]; ok {
		return check(base, args[1:])
	}
	return nil
}

// builtAtRunTime: an expansion, a glob or braces make the name; [ and [[
// are the test commands.
func builtAtRunTime(name string) bool {
	return name == dynamic || strings.ContainsAny(name, "{*?") || strings.Contains(name, "[") && name != "[" && name != "[["
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
}

// unwrap drops leading wrappers (env, timeout, xargs...) and their options,
// leaving the command they run. env -S's text is a command line itself.
func unwrap(args []string) []string {
	for len(args) > 0 {
		valued, ok := wrappers[path.Base(args[0])]
		if !ok {
			return args
		}
		env := path.Base(args[0]) == "env"
		args = args[1:]
		for len(args) > 0 && isOption(args[0], env) {
			if text, split := splitString(args); split {
				return []string{"sh", "-c", text}
			}
			if has(args[:1], valued...) && !strings.Contains(args[0], "=") {
				args = args[1:]
			}
			args = args[min(1, len(args)):]
		}
	}
	return args
}

// isOption: an option, an env assignment, or timeout's duration.
func isOption(a string, env bool) bool {
	return strings.HasPrefix(a, "-") || env && strings.Contains(a, "=") || isDuration(a)
}

// splitString recognises env -S TEXT (or --split-string=TEXT) and returns
// TEXT with the words after it.
func splitString(args []string) (string, bool) {
	a := args[0]
	switch {
	case a == "-S" || a == "--split-string":
		if len(args) < 2 {
			return dynamic, true
		}
		return strings.Join(args[1:], " "), true
	case strings.HasPrefix(a, "--split-string="):
		return strings.Join(append([]string{strings.TrimPrefix(a, "--split-string=")}, args[1:]...), " "), true
	case strings.HasPrefix(a, "-S") && len(a) > 2:
		return strings.Join(append([]string{a[2:]}, args[1:]...), " "), true
	}
	return "", false
}

// scriptDanger checks the text trap runs on a signal, or watch runs
// repeatedly (its command words joined, as watch passes them to sh -c).
func scriptDanger(base string, args []string, depth int) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if base == "watch" && has(args[:1], "-n", "--interval", "-d", "--differences") && !strings.Contains(args[0], "=") {
			args = args[1:]
		}
		args = args[min(1, len(args)):]
	}
	if len(args) == 0 {
		return nil
	}
	text := args[0]
	if base == "watch" {
		text = strings.Join(args, " ")
	}
	if strings.Contains(text, dynamic) {
		return []string{base + " runs text built at run time"}
	}
	return dangerous(text, depth+1)
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
		if a == dynamic || has([]string{a}, "--recursive") || shortFlag(a, 'r') || shortFlag(a, 'R') {
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
	"--namespace": true, "--exec-path": true, "--config-env": true, "--attr-source": true, "--super-prefix": true}

func gitDanger(args []string, depth int) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if gitAlias(args) {
			return []string{"git -c alias defines a command the check cannot see"}
		}
		if gitGlobalWithValue[args[0]] {
			args = args[1:]
		}
		args = args[min(1, len(args)):]
	}
	if len(args) == 0 {
		return nil
	}
	return gitSubDanger(args[0], args[1:], depth)
}

func gitSubDanger(sub string, rest []string, depth int) []string {
	switch {
	case sub == dynamic:
		return []string{"the git command is built at run time"}
	case sub == "submodule":
		return gitForeach(rest, depth)
	case sub == "bisect" && len(rest) > 1 && rest[0] == "run":
		return callDanger(rest[1:], depth+1)
	}
	if check, ok := gitChecks[sub]; ok {
		if why := check(rest); why != "" {
			return []string{why}
		}
	}
	return nil
}

// gitAlias: -c alias.x=... (or --config-env=alias.x=VAR) defines what x
// runs, which could be anything.
func gitAlias(args []string) bool {
	a := strings.ToLower(args[0])
	switch {
	case (a == "-c" || a == "--config-env") && len(args) > 1:
		return strings.HasPrefix(strings.ToLower(args[1]), "alias.") || args[1] == dynamic
	case strings.HasPrefix(a, "--config-env="):
		return strings.HasPrefix(strings.TrimPrefix(a, "--config-env="), "alias.")
	}
	return false
}

// gitForeach checks the command git submodule foreach runs in each
// submodule.
func gitForeach(args []string, depth int) []string {
	if len(args) == 0 || args[0] != "foreach" {
		return nil
	}
	args = args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = args[1:]
	}
	text := strings.Join(args, " ")
	if strings.Contains(text, dynamic) {
		return []string{"git submodule foreach runs text built at run time"}
	}
	return dangerous(text, depth+1)
}

// has reports whether args holds one of names. A long option also matches
// by a unique-looking prefix (--har is --hard to git and GNU tools) and
// with an attached value (--force=1).
func has(args []string, names ...string) bool {
	for _, a := range args {
		opt, _, _ := strings.Cut(a, "=")
		for _, n := range names {
			if a == n || strings.HasPrefix(n, "--") && strings.HasPrefix(opt, "--") && len(opt) > 2 && strings.HasPrefix(n, opt) {
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
		return when(has(a, "--", ".", "--force", "--discard-changes", "--ours", "--theirs") || hasShort(a, 'f') || operands(a) > 1,
			"git checkout over files discards their changes")
	},
	"prune": func([]string) string { return "git prune drops unreachable work" },
	"worktree": func(a []string) string {
		return when(len(a) > 0 && a[0] == "remove" && (has(a, "--force") || hasShort(a, 'f')), "git worktree remove --force deletes a working tree")
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

// operands counts the arguments that are not options.
func operands(args []string) int {
	n := 0
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			n++
		}
	}
	return n
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
