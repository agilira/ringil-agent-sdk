// security_test.go: fuzz and path-normalization evasion tests for file policies
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"runtime"
	"testing"
)

func FuzzPolicy(f *testing.F) {
	seeds := []string{
		"/repo/.env",
		"/repo/.ENV",
		"/repo/.env.",
		"/repo/.env ",
		"/repo/.env:Zone.Identifier",
		"/repo/.env.example",
		"/repo/.ssh/id_rsa",
		"/repo/.git/hooks/pre-commit",
		"/repo/AGENTS.md:ads",
		"/repo/id_r\u017fa",
		"/repo/foo.\u212aey",
		`C:\repo\.env`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	exPol, err := newPolicy(DefaultExclude)
	if err != nil {
		f.Fatal(err)
	}
	prPol, err := newPolicy(DefaultProtected)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if len(p) > 2048 {
			return
		}
		_ = Excluded(p, nil)
		_ = Protected(p, nil)
		_ = normalize(p)
		if exPol.excludedLeaf(p) && !exPol.excluded(p) {
			t.Fatalf("excludedLeaf(%q) true while excluded false", p)
		}
		if prPol.excludedLeaf(p) && !prPol.excluded(p) {
			t.Fatalf("protectedLeaf(%q) true while protected false", p)
		}
	})
}

func TestSecurityPolicyEvasionVariants(t *testing.T) {
	mustExclude := []string{
		"/repo/.env",
		"/repo/.ENV",
		"/repo/.env.",
		"/repo/.env   ",
		"/repo/.env:Zone.Identifier",
		"/repo/.env.local",
		"/repo/.env.production",
		"/home/u/.SSH/id_ed25519",
		"/home/u/id_r\u017fa",
		"/home/u/cert.\u212aey",
		"/home/u/.config/git/credentials",
		"/home/u/.config/gcloud/adc.json",
		"/home/u/.claude/.credentials.json",
		"/home/u/.codex/auth.json",
	}
	if runtime.GOOS == "windows" {
		mustExclude = append(mustExclude, `C:\work\.env`, `C:\work\.aws\credentials`)
	}
	for _, p := range mustExclude {
		if !Excluded(p, nil) {
			t.Errorf("expected excluded: %q", p)
		}
	}
	mustShow := []string{
		"/repo/.env.example",
		"/repo/.env.sample",
		"/repo/.env.template",
		"/repo/.env.dist",
		"/repo/main.go",
	}
	for _, p := range mustShow {
		if Excluded(p, nil) {
			t.Errorf("template/source wrongly excluded: %q", p)
		}
	}
	mustProtect := []string{
		"/repo/AGENTS.md",
		"/repo/AGENTS.md.",
		"/repo/AGENTS.md:ads",
		"/repo/cLaUdE.mD",
		"/repo/.git/hooks/pre-commit",
		"/repo/.github/workflows/ci.yml",
		"/repo/.mcp.json",
		"/repo/.envrc",
		"/repo/.vscode/settings.json",
	}
	for _, p := range mustProtect {
		if !Protected(p, nil) {
			t.Errorf("expected protected: %q", p)
		}
	}
}
