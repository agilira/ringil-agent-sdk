// permission_match.go: commands, paths and hosts as the rules see them
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/url"
	"path"
	"strings"
)

// shellControls are what join, nest or redirect commands: a command with
// any of them is not one command.
var shellControls = []string{";", "&", "|", "\n", "\r", "`", "$(", "<(", ">(", ">", "<", "(", ")", "{", "}"}

// launchers are programs that run another program or code they are given:
// an allow rule never stands for them, nor does an "always" learn them.
var launchers = map[string]bool{
	"sudo": true, "doas": true, "su": true, "env": true, "nohup": true, "nice": true, "ionice": true, "timeout": true,
	"time": true, "watch": true, "xargs": true, "find": true, "exec": true, "eval": true, "command": true, "builtin": true,
	"source": true, ".": true, "bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true,
	"tcsh": true, "python": true, "python3": true, "node": true, "deno": true, "bun": true, "perl": true, "ruby": true,
	"php": true, "lua": true, "awk": true, "gawk": true, "sed": true, "ssh": true, "parallel": true, "busybox": true,
}

// plainCommand: one command whose words are what they look like: nothing
// joined, nested or redirected, no expansion, quote or escape in its
// program, no variable set before it, and a program that runs nothing
// else. Only such a command can be allowed by a rule of its words.
func plainCommand(command string) bool {
	for _, c := range shellControls {
		if strings.Contains(command, c) {
			return false
		}
	}
	if strings.ContainsAny(command, "$\\") {
		return false // ${x:=$(id)} and ${x@P} run code with no $( in sight
	}
	words := strings.Fields(command)
	if len(words) == 0 {
		return false
	}
	first := words[0]
	return !strings.ContainsAny(first, `='"/*?[`) && !launchers[first]
}

// commandMatches: the rule's words on command. Allow: a plain command,
// starting with them (pattern:*) or exactly them. Deny and ask: any part
// of the command where the rule's first word is a word (its quotes and
// escapes taken off, a program's path to its name, a glob that could be
// it) and its other words follow, options between them or not. A command
// whose words an expansion hides matches no deny rule: the policy asks
// about it instead (Policy.Effect). Matching a shell command by its text is
// best effort: what a command decodes and runs (base64 -d | sh) no rule
// sees; the sandbox is the boundary.
func (r Rule) commandMatches(command string) bool {
	want, prefix := strings.CutSuffix(r.Pattern, ":*")
	wantWords := strings.Fields(want)
	if r.Effect == EffectAllow {
		words := strings.Fields(command)
		return plainCommand(command) && hasWordsAt(words, 0, wantWords) && (prefix || len(words) == len(wantWords))
	}
	return anyPartHas(command, wantWords)
}

// unreadable: an expansion or a continued line may hide which program a
// part runs: a $ in a part's program ($a$b, ${IFS}) or anywhere in what a
// launcher runs (bash -c "$x"), a continued line (r\<newline>m). An
// argument ($HOME of echo) hides no program.
func unreadable(command string) bool {
	if strings.Contains(command, "\\\n") {
		return true
	}
	for _, part := range strings.FieldsFunc(command, func(r rune) bool { return strings.ContainsRune(";&|\n\r", r) }) {
		if hiddenProgram(part) {
			return true
		}
	}
	return false
}

// hiddenProgram: an expansion is, or feeds, the program a part runs:
// $(printf r)m, $a$b, bash -c "$x".
func hiddenProgram(part string) bool {
	words := strings.Fields(part)
	for len(words) > 0 && strings.Contains(words[0], "=") && !strings.ContainsAny(words[0], "$`") {
		words = words[1:] // X=1 before the program
	}
	if len(words) == 0 {
		return false
	}
	return strings.ContainsAny(words[0], "$`") || launchers[words[0]] && strings.ContainsAny(part, "$`")
}

// anyPartHas: some part of the command has want's first word, and its
// other words after it in order.
func anyPartHas(command string, want []string) bool {
	if len(want) == 0 {
		return false
	}
	for _, part := range commandParts(command) {
		words := looseWords(part)
		for i, w := range words {
			if sameWord(w, want[0]) && inOrder(words[i+1:], want[1:]) {
				return true
			}
		}
	}
	return false
}

// sameWord: w is want, or a glob that could be it (/bin/r? is rm).
func sameWord(w, want string) bool {
	if w == want {
		return true
	}
	ok, _ := path.Match(w, want)
	return ok && strings.ContainsAny(w, "*?[")
}

// inOrder: want's words appear in words, in order, with others between.
func inOrder(words, want []string) bool {
	for _, w := range words {
		if len(want) == 0 {
			break
		}
		if sameWord(w, want[0]) {
			want = want[1:]
		}
	}
	return len(want) == 0
}

// commandParts are the commands a compound command runs, roughly: split at
// every control, so that a deny rule finds a part wherever it hides.
func commandParts(command string) []string {
	parts := []string{command}
	for _, c := range shellControls {
		var next []string
		for _, p := range parts {
			next = append(next, strings.Split(p, c)...)
		}
		parts = next
	}
	return parts
}

// looseWords are a part's words with quotes and escapes taken off, and a
// program's path to its name: "rm", r”m, \rm and /bin/rm are rm.
func looseWords(part string) []string {
	words := strings.Fields(strings.NewReplacer(`"`, "", `'`, "", `\`, "", ",", " ").Replace(part)) // r''m is rm; {rm,-rf,x} is rm -rf x
	for i, w := range words {
		if j := strings.LastIndex(w, "/"); j >= 0 && j < len(w)-1 {
			words[i] = w[j+1:]
		}
	}
	return words
}

// hasWordsAt: words[i:] starts with want.
func hasWordsAt(words []string, i int, want []string) bool {
	if len(want) == 0 || len(words)-i < len(want) {
		return false
	}
	for j := range want {
		if words[i+j] != want[j] {
			return false
		}
	}
	return true
}

// pathMatches: allow, the path as given, inside the folder the pattern
// names (a relative pattern never reaches an absolute path or one above);
// deny and ask, the path or any tail of it, lowercased too, and as Windows
// reads it (.env., ".env ", .env::$DATA are .env).
func (r Rule) pathMatches(p string) bool {
	clean := cleanPath(p)
	if r.Effect == EffectAllow {
		return !outside(r.Pattern, clean) && globMatch(r.Pattern, clean)
	}
	for _, candidate := range []string{clean, strings.ToLower(clean), windowsName(strings.ToLower(clean))} {
		parts := strings.Split(strings.TrimPrefix(candidate, "/"), "/")
		for i := range parts {
			if globMatch(strings.ToLower(r.Pattern), strings.Join(parts[i:], "/")) || globMatch(r.Pattern, strings.Join(parts[i:], "/")) {
				return true
			}
		}
	}
	return false
}

// outside: p is absolute or above, and the pattern does not say so.
func outside(pattern, p string) bool {
	above := p == ".." || strings.HasPrefix(p, "../")
	pattern = cleanPath(pattern)
	return (path.IsAbs(p) && !path.IsAbs(pattern)) || (above && !strings.HasPrefix(pattern, ".."))
}

// windowsName is p as Windows resolves each name: trailing dots and
// spaces dropped, an alternate stream (name:stream, name::$DATA) cut.
func windowsName(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if j := strings.Index(part, ":"); j > 1 { // C: stays
			part = part[:j]
		}
		parts[i] = strings.TrimRight(part, ". ")
	}
	return strings.Join(parts, "/")
}

// cleanPath is p with slashes, cleaned.
func cleanPath(p string) string { return path.Clean(strings.ReplaceAll(p, `\`, "/")) }

// hostMatches: the URL's host is domain or under it. Allow: an ASCII
// host as read. Deny and ask: also a host that cannot be read, or one in
// another script or its punycode (it may spell the same name).
func (r Rule) hostMatches(domain, raw string) bool {
	host, ok := urlHost(raw)
	if !ok {
		return r.Effect != EffectAllow
	}
	if r.Effect != EffectAllow && (strings.Contains(host, "xn--") || !isASCII(host)) {
		return true
	}
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimPrefix(domain, "*.")), ".")
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// urlHost is the URL's host, lowercase, without a final dot.
func urlHost(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// globMatch matches a slash path against a pattern where ** spans folders
// and * one name.
func globMatch(pattern, name string) bool {
	return matchParts(strings.Split(path.Clean(pattern), "/"), strings.Split(name, "/"))
}

func matchParts(pattern, name []string) bool {
	pi, ni, starP, starN := 0, 0, -1, 0
	for ni < len(name) {
		if pi < len(pattern) && pattern[pi] == "**" {
			starP, starN = pi, ni+1
			pi++
			continue
		}
		if pi < len(pattern) && segMatch(pattern[pi], name[ni]) {
			pi, ni = pi+1, ni+1
			continue
		}
		if starP < 0 {
			return false
		}
		pi, ni = starP+1, starN
		starN++
	}
	return allStars(pattern[pi:])
}

func segMatch(pattern, seg string) bool {
	ok, _ := path.Match(pattern, seg)
	return ok
}

func allStars(parts []string) bool {
	for _, p := range parts {
		if p != "**" {
			return false
		}
	}
	return true
}

// commandWords is a command's words, joined by one space.
func commandWords(command string) string { return strings.Join(strings.Fields(command), " ") }
