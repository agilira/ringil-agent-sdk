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
	".git-credentials", ".npmrc", ".pypirc", ".gem/credentials", ".cargo/credentials*",
	".terraform.d/credentials*", ".m2/settings*.xml",
	// password stores, keyrings and browser profiles
	".password-store", ".local/share/keyrings", "Keychains",
	".mozilla", ".config/google-chrome", ".config/chromium", ".config/BraveSoftware", ".config/microsoft-edge",
	"Library/Application Support/Google/Chrome", "Library/Application Support/Firefox",
	// AI tools' own credentials
	".claude/.credentials.json", ".codex/auth.json",
}

var templateSuffixes = []string{".example", ".sample", ".template", ".dist"}

// policy decides which paths are excluded.
type policy struct {
	patterns [][]string // each pattern split into lower-cased components
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

func matchAt(pat, parts []string) bool {
	if len(parts) < len(pat) {
		return false
	}
	for j, pp := range pat {
		if ok, err := path.Match(pp, parts[j]); err != nil || !ok {
			return false
		}
	}
	return !isTemplate(parts[len(pat)-1])
}

func isTemplate(name string) bool {
	for _, s := range templateSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
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

// normalize lower-cases a name and drops trailing dots and spaces, which
// Windows ignores: ".env." and ".env " open .env there.
func normalize(name string) string {
	return strings.TrimRight(strings.ToLower(name), ". ")
}
