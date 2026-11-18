// policy.go: files the tools never read
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"path"
	"path/filepath"
	"strings"
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
	".git-credentials", ".npmrc", "etc/npmrc", ".pypirc", ".gem/credentials", ".cargo/credentials*",
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
	patterns [][]string // each pattern split into lower-cased components
	longest  int        // components in the longest pattern
}

func newPolicy(patterns []string) (policy, error) {
	p := policy{}
	for _, raw := range patterns {
		parts := strings.Split(strings.ToLower(strings.Trim(raw, "/")), "/")
		for _, part := range parts {
			if _, err := path.Match(part, ""); err != nil || part == "" {
				return policy{}, err
			}
		}
		p.patterns = append(p.patterns, parts)
		p.longest = max(p.longest, len(parts))
	}
	return p, nil
}

// excluded reports whether the absolute path p matches a pattern anywhere
// along it.
func (p policy) excluded(abs string) bool {
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
	parts := tail(abs, p.longest)
	for _, pat := range p.patterns {
		if len(pat) <= len(parts) && matchAt(pat, parts[len(parts)-len(pat):]) {
			return true
		}
	}
	return false
}

func matchAt(pat, parts []string) bool {
	if len(parts) < len(pat) {
		return false
	}
	for j, pp := range pat {
		if !matchPart(pp, parts[j]) {
			return false
		}
	}
	return !isTemplate(parts[len(pat)-1])
}

// matchPart is path.Match with fast paths for the common shapes: a plain
// name, "prefix*" and "*suffix". A walk calls it for every entry and
// pattern.
func matchPart(pat, name string) bool {
	meta := strings.IndexAny(pat, "*?[\\")
	switch {
	case meta < 0:
		return pat == name
	case meta == len(pat)-1 && pat[meta] == '*':
		return strings.HasPrefix(name, pat[:meta])
	case meta == 0 && pat[0] == '*' && !strings.ContainsAny(pat[1:], "*?[\\"):
		return strings.HasSuffix(name, pat[1:])
	}
	ok, err := path.Match(pat, name)
	return err == nil && ok
}

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
	return strings.TrimRight(strings.ToLower(name), ". ")
}
