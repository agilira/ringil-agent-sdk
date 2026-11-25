// policy.go: files the tools never read
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"github.com/agilira/ringil-agent-sdk/dirs"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

// DefaultExclude are patterns of paths the tools never show or read,
// because what a tool reads is sent to the model's provider. A pattern
// without "/" matches any single path component (".ssh" hides the whole
// directory); a pattern with "/" matches consecutive components
// (".config/gh"). Matching ignores case, since macOS and Windows file
// systems do. Templates such as .env.example stay visible.
var DefaultExclude = []string{
	// environment files and shell credentials
	".env", ".env.*", ".netrc", ".pgpass", ".my.cnf", ".vault-token",
	// keys and certificates
	".ssh", ".gnupg", "*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.kdbx",
	"id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*",
	// cloud, cluster and package registry credentials
	".aws", ".azure", ".kube", ".docker", ".config/gcloud", ".config/gh", ".config/hub",
	".git-credentials", ".config/git/credentials", ".npmrc", "etc/npmrc", ".pypirc", ".gem/credentials", ".cargo/credentials*",
	".terraform.d/credentials*", ".m2/settings*.xml",
	// password stores, keyrings and browser profiles
	".password-store", ".local/share/keyrings", "Keychains",
	".mozilla", ".config/google-chrome", ".config/chromium", ".config/BraveSoftware", ".config/microsoft-edge",
	"Library/Application Support/Google/Chrome", "Library/Application Support/Firefox",
	// AI tools' own credentials
	".claude/.credentials.json", ".codex/auth.json",
}

// Excluded reports whether the absolute path abs matches the exclusion
// patterns (DefaultExclude when nil): the same test the file tools apply.
func Excluded(abs string, patterns []string) bool {
	if patterns == nil {
		patterns = DefaultExclude
	}
	pol, err := newPolicy(patterns)
	return err != nil || pol.excluded(abs)
}

// DefaultProtected are paths that hold people's own instructions and tool
// settings: other agents' rules, editor and CI configuration, the
// repository itself. A tool that writes files must confirm every change
// to them with the user, whatever was allowed before; reading is free.
var DefaultProtected = []string{
	// agents' rules
	"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md", "GEMINI.md",
	".cursorrules", ".windsurfrules", ".clinerules", ".github/copilot-instructions.md",
	// agents' and editors' settings
	".claude", ".codex", ".cursor", ".gemini", ".windsurf", ".continue", ".aider*", ".vscode", ".idea", ".zed", ".devcontainer",
	// the repository and its automation
	".git", ".github", ".gitlab-ci.yml", ".gitignore", ".gitattributes", ".gitmodules", ".pre-commit-config.yaml", ".husky",
	// files that make other tools run code: MCP servers, direnv
	".mcp.json", ".envrc",
	// what the shell and the desktop run on the host: startup files and
	// autostart entries (a home granted writable would otherwise let a
	// command leave code for the next login)
	".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".profile", ".zshrc", ".zprofile", ".zshenv", ".zlogin",
	".config/fish/config.fish", ".config/autostart", ".config/systemd/user", "Library/LaunchAgents",
}

// Protected reports whether abs matches the protected patterns
// (DefaultProtected when nil).
func Protected(abs string, patterns []string) bool {
	if patterns == nil {
		patterns = DefaultProtected
	}
	pol, err := newPolicy(patterns)
	return err != nil || pol.excluded(abs)
}

var templateSuffixes = []string{".example", ".sample", ".template", ".dist"}

// policy decides which paths are excluded.
type policy struct {
	patterns [][]namePattern // each pattern split into lower-cased components
	longest  int             // components in the longest pattern
	roots    []string        // whole directories, by their real path
}

func newPolicy(patterns []string) (policy, error) {
	p := policy{}
	for _, raw := range patterns {
		parts := strings.Split(fold(strings.Trim(raw, "/")), "/")
		for _, part := range parts {
			if _, err := path.Match(part, ""); err != nil || part == "" {
				return policy{}, err
			}
		}
		compiled := make([]namePattern, len(parts))
		for i, part := range parts {
			compiled[i] = compilePart(part)
		}
		p.patterns = append(p.patterns, compiled)
		p.longest = max(p.longest, len(parts))
	}
	return p, nil
}

// excluded reports whether the absolute path p matches a pattern anywhere
// along it.
func (p policy) excluded(abs string) bool {
	if p.inRoot(abs) {
		return true
	}
	parts := components(abs)
	for i := range parts {
		for _, pat := range p.patterns {
			if matchAt(pat, parts[i:]) {
				return true
			}
		}
	}
	return false
}

// excludedLeaf reports whether a pattern ends at the last component of
// abs: enough during a walk, where every ancestor was checked already.
func (p policy) excludedLeaf(abs string) bool {
	if p.inRoot(abs) {
		return true
	}
	parts := tail(abs, p.longest)
	for _, pat := range p.patterns {
		if len(pat) <= len(parts) && matchAt(pat, parts[len(parts)-len(pat):]) {
			return true
		}
	}
	return false
}

// inRoot: abs is one of the whole directories excluded, or inside one.
func (p policy) inRoot(abs string) bool {
	for _, r := range p.roots {
		if dirs.Within(abs, r) {
			return true
		}
	}
	return false
}

// withRoots adds whole directories to p, by their real path where they
// exist.
func (p policy) withRoots(roots []string) policy {
	for _, r := range roots {
		if r == "" || !filepath.IsAbs(r) {
			continue
		}
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		p.roots = append(p.roots, filepath.Clean(r))
	}
	return p
}

func matchAt(pat []namePattern, parts []string) bool {
	if len(parts) < len(pat) {
		return false
	}
	for j, pp := range pat {
		if !pp.match(parts[j]) {
			return false
		}
	}
	return !isTemplate(parts[len(pat)-1])
}

// namePattern is one component of a pattern, its shape found once: a walk
// matches every entry against every pattern.
type namePattern struct {
	kind byte   // '=' a plain name, '>' "prefix*", '<' "*suffix", 0 any other glob
	fix  string // the name, prefix or suffix
	raw  string
}

func compilePart(pat string) namePattern {
	meta := strings.IndexAny(pat, "*?[\\")
	switch {
	case meta < 0:
		return namePattern{kind: '=', fix: pat, raw: pat}
	case meta == len(pat)-1 && pat[meta] == '*':
		return namePattern{kind: '>', fix: pat[:meta], raw: pat}
	case meta == 0 && pat[0] == '*' && !strings.ContainsAny(pat[1:], "*?[\\"):
		return namePattern{kind: '<', fix: pat[1:], raw: pat}
	}
	return namePattern{raw: pat}
}

// match is path.Match, with fast paths for the common shapes.
func (np namePattern) match(name string) bool {
	switch np.kind {
	case '=':
		return np.fix == name
	case '>':
		return strings.HasPrefix(name, np.fix)
	case '<':
		return strings.HasSuffix(name, np.fix)
	}
	ok, err := path.Match(np.raw, name)
	return err == nil && ok
}

// matchPart matches one pattern component against a name.
func matchPart(pat, name string) bool { return compilePart(pat).match(name) }

func isTemplate(name string) bool {
	for _, s := range templateSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// tail returns the last n normalized names of abs, without splitting the
// rest: a walk asks this for every entry.
func tail(abs string, n int) []string {
	rest := filepath.ToSlash(abs)
	parts := make([]string, 0, n)
	for len(parts) < n && rest != "" {
		i := strings.LastIndexByte(rest, '/')
		if name := rest[i+1:]; name != "" {
			parts = append(parts, normalize(name))
		}
		if i < 0 {
			break
		}
		rest = rest[:i]
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return parts
}

// components splits an absolute path into normalized names.
func components(abs string) []string {
	var parts []string
	for _, part := range strings.Split(filepath.ToSlash(abs), "/") {
		if part != "" {
			parts = append(parts, normalize(part))
		}
	}
	return parts
}

// normalize lower-cases a name and drops what Windows ignores or treats
// as part of the same file: trailing dots and spaces (".env." opens .env)
// and an alternate data stream ("AGENTS.md:x" writes to AGENTS.md).
func normalize(name string) string {
	if i := strings.IndexByte(name, ':'); i > 0 {
		name = name[:i]
	}
	return strings.TrimRight(fold(name), ". ")
}

func ascii(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// asciiLower is fold for an ASCII letter. 'K' and 'S' fold with non-ASCII
// letters (Kelvin, long s) to their lower case too: the same result.
func asciiLower(r rune) rune {
	if 'A' <= r && r <= 'Z' {
		return r + 'a' - 'A'
	}
	return r
}

// fold maps every letter to one form per Unicode case class: the file
// systems that ignore case treat "ſ" as "s" and "K" (Kelvin) as "k",
// which lower-casing alone does not.
func fold(s string) string {
	if ascii(s) { // the common case, fast: a walk folds every name
		return strings.Map(asciiLower, s)
	}
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return unicode.ToLower(least)
	}, s)
}
