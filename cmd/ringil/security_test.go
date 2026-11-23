// security_test.go: fuzz and validation/hardening security tests for ringil.yaml
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/argus"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/secrets"
)

func FuzzConfigYAML(f *testing.F) {
	seeds := []string{
		fullConfig,
		"model: test\n",
		"modle: x\n",
		"mcp_servers:\n  f: {comand: x}\n",
		"model: a\n---\nmodel: b\n",
		"defaults: &d\n  model: x\nprofiles:\n  w: *d\n",
		"profiles: null\n",
		`instructions: "*.md"` + "\n",
		"transcribe:\n  provider: anthropic\n",
		"proxy: {url: \"http://u:p@proxy:3128\"}\n",
		"proxy: {no_proxy: [x]}\n",
		"providers:\n  x: {base: \"https://u:p@h/v1\", key_env: \"A B\"}\n",
		"providers:\n  openai: {kind: anthropic}\nprovider: openai\n",
		"provider: x\nprofiles:\n  w: {provider: y, embed_model: e}\n",
		"serve:\n  max_runs: -1\n",
		"serve:\n  replies: shout\n",
		"serve:\n  language: fr\n",
		"serve:\n  no_triggers: true\n  webhooks: 127.0.0.1:1\n",
		"serve:\n  new_after: soon\n",
		"profiles:\n  default: {}\n",
		"mcp_servers:\n  f: {command: x, url: https://example.com}\n",
		"\x00\xff\xfe",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	base := f.TempDir()
	var seq atomic.Uint64
	var dirs sync.Pool
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 8192 {
			return
		}
		_, _ = argus.ParseConfig(raw, argus.FormatYAML)
		dir := borrowFuzzDir(&dirs, base, &seq)
		defer dirs.Put(dir)
		fuzzBindConfig(t, *dir, raw)
	})
}

func borrowFuzzDir(pool *sync.Pool, base string, seq *atomic.Uint64) *string {
	if v := pool.Get(); v != nil {
		return v.(*string)
	}
	dir := filepath.Join(base, strconv.FormatUint(seq.Add(1), 10))
	_ = os.MkdirAll(dir, 0o700)
	return &dir
}

func fuzzBindConfig(t *testing.T, dir string, raw []byte) {
	t.Helper()
	path := filepath.Join(dir, "ringil.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return
	}
	watched, err := argus.Setup("ringil").File(path).MaxStaleness(time.Hour).Start()
	if err != nil {
		return
	}
	defer func() { _ = watched.Close() }()
	bound, err := argus.Bind[config](watched, argus.Strict())
	if err != nil {
		return
	}
	cfg := bound.Value()
	resolved := cfg.from(dir)
	_ = resolved.shortDigest()
	_ = resolved.Serve.people(false)
	_ = restartFor(resolved, resolved)
	checkAcceptedConfig(t, cfg)
}

func checkAcceptedConfig(t *testing.T, cfg *config) {
	t.Helper()
	if strings.ContainsAny(cfg.Instructions, "*?[") {
		t.Fatalf("glob instructions accepted: %q", cfg.Instructions)
	}
	checkAcceptedProfiles(t, cfg)
	checkAcceptedServe(t, cfg)
	for name, p := range cfg.Providers {
		if checkProvider(name, p) != nil {
			t.Fatalf("bad provider %q accepted: %+v", name, p)
		}
	}
	for name, m := range cfg.MCPServers {
		if m.check() != nil {
			t.Fatalf("bad mcp_server %q accepted: %+v", name, m)
		}
	}
}

func checkAcceptedProfiles(t *testing.T, cfg *config) {
	t.Helper()
	for name, p := range cfg.Profiles {
		if name == "default" || p.MaxTurns < 0 || strings.ContainsAny(p.Instructions, "*?[") {
			t.Fatalf("bad profile %q accepted: %+v", name, p)
		}
		if p.Memory != "" && p.Memory != "chat" && p.Memory != "off" {
			t.Fatalf("bad profile memory %q accepted in %q", p.Memory, name)
		}
	}
}

func checkAcceptedServe(t *testing.T, cfg *config) {
	t.Helper()
	s := cfg.Serve
	if s.MaxRuns < 0 || (s.NoTriggers && s.Webhooks != "") {
		t.Fatalf("bad serve config accepted: %+v", s)
	}
	if !slices.Contains([]string{"", "voice", "text"}, s.Replies) || !slices.Contains([]string{"", "en", "it"}, s.Language) {
		t.Fatalf("bad serve replies/lang accepted: %+v", s)
	}
	checkChatRoles(t, cfg.Profiles, s.MemberProfile, s.PublicProfile)
}

func checkChatRoles(t *testing.T, profiles map[string]profile, names ...string) {
	t.Helper()
	for _, roleName := range names {
		if roleName == "" {
			continue
		}
		p, ok := profiles[roleName]
		if !ok || p.Tools == nil || (p.Memory != "chat" && p.Memory != "off") {
			t.Fatalf("bad chat profile %q accepted: %+v", roleName, p)
		}
	}
}

func TestSecurityConfigValidationBoundaries(t *testing.T) {
	badConfigs := map[string]config{
		"globGlobal":    {Instructions: "*.md"},
		"qmarkGlobal":   {Instructions: "a?b.md"},
		"bracketGlobal": {Instructions: "[a].md"},
		"globProfile":   {Profiles: map[string]profile{"w": {Instructions: "*.md"}}},
		"badTranscribe": {Transcribe: audioConfig{Provider: "nowhere"}},
		"badSpeech":     {Speech: audioConfig{Provider: "chatgpt"}},
		"badProvKey":    {Providers: map[string]providerEntry{"w": {Base: "https://a.test", KeyEnv: "A B"}}},
		"provNoBase":    {Providers: map[string]providerEntry{"w": {KeyEnv: "MY_KEY"}}},
		"provBadName":   {Providers: map[string]providerEntry{"W x": {Base: "https://a.test"}}},
		"provBadKind":   {Providers: map[string]providerEntry{"w": {Kind: "grpc", Base: "https://a.test"}}},
		"provRekind":    {Providers: map[string]providerEntry{"openai": {Kind: providerAnthropic}}},
		"provCreds":     {Providers: map[string]providerEntry{"w": {Base: "https://u:p@a.test"}}},
		"provScheme":    {Providers: map[string]providerEntry{"w": {Base: "file:///etc"}}},
		"chatgptKey":    {Providers: map[string]providerEntry{"chatgpt": {KeyEnv: "K"}}},
		"mainUnknown":   {Provider: "nowhere"},
		"roleUnknown":   {Profiles: map[string]profile{"w": {Provider: "nowhere"}}},
		"embedNoProv":   {Profiles: map[string]profile{"w": {EmbedModel: "emb"}}},
		"embedClaude":   {Profiles: map[string]profile{"w": {Provider: "anthropic", EmbedModel: "emb"}}},
		"mcpBadType":    {MCPServers: map[string]mcpEntry{"m": {Type: "grpc", Command: "x"}}},
		"mcpCmdAndURL":  {MCPServers: map[string]mcpEntry{"m": {Command: "x", URL: "https://x.test"}}},
	}
	for name, cfg := range badConfigs {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate accepted invalid config", name)
		}
	}
	for _, d := range []string{"", "abc", strings.Repeat("a", 64)} {
		if got := (&config{digest: d}).shortDigest(); len(got) > 12 || got != d[:min(12, len(d))] {
			t.Errorf("shortDigest(%q) = %q", d, got)
		}
	}
	checkProtectedFilesList(t)
}

func checkProtectedFilesList(t *testing.T) {
	t.Helper()
	h := newHarness(t, "")
	writeConfig(t, h.dir, fullConfig)
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		got := h.c.protectedFiles(ctx)
		root := filepath.Join(h.dir, "ringil")
		for _, want := range []string{
			filepath.Join(root, "ringil.yaml"),
			filepath.Join(root, "company.md"),
			filepath.Join(root, "support.md"),
		} {
			if !slices.Contains(got, want) {
				t.Errorf("protectedFiles missing %q in %v", want, got)
			}
		}
		return nil
	})
}

func TestSecurityInstructionsFileHardening(t *testing.T) {
	for name, content := range map[string][]byte{
		"oversized":   []byte(strings.Repeat("a", agent.MaxInstructions+1)),
		"invalidUTF8": []byte("ok\xff"),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "", fake.Reply("ok"))
			writeConfig(t, h.dir, fullConfig)
			inst := filepath.Join(h.dir, "ringil", "company.md")
			if err := os.WriteFile(inst, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if code := h.run("--no-tools", "run", "hi"); code == 0 {
				t.Errorf("%s instructions file accepted", name)
			}
		})
	}
	checkInstructionsPermsNonWindows(t)
}

func checkInstructionsPermsNonWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	h := newHarness(t, "", fake.Reply("ok"))
	writeConfig(t, h.dir, fullConfig)
	inst := filepath.Join(h.dir, "ringil", "company.md")
	if err := os.Chmod(inst, 0o666); err != nil {
		t.Fatal(err)
	}
	if code := h.run("--no-tools", "run", "hi"); code == 0 {
		t.Error("world-writable instructions file accepted")
	}

	h2 := newHarness(t, "", fake.Reply("ok"))
	writeConfig(t, h2.dir, fullConfig)
	inst2 := filepath.Join(h2.dir, "ringil", "company.md")
	target := filepath.Join(t.TempDir(), "real.md")
	if err := os.WriteFile(target, []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(inst2); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, inst2); err != nil {
		t.Fatal(err)
	}
	if code := h2.run("--no-tools", "run", "hi"); code == 0 {
		t.Error("symlink instructions file accepted")
	}
}

// Any address the setup accepts names a provider ringil.yaml takes, with a
// key name the stores take.
func FuzzServerName(f *testing.F) {
	for _, s := range []string{"https://vllm.lan:8000/v1", "http://10.0.0.5/v1", "http://[::1]:8000/v1", "https://" + strings.Repeat("a.", 60) + "x/v1", "https://-/v1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, base string) {
		if u, err := url.Parse(base); err != nil || u.Host == "" {
			return
		}
		name := serverName(base)
		if !providerName.MatchString(name) || secrets.ValidateName(keyNameOf(name)) != nil {
			t.Fatalf("%q: name %q, key %q", base, name, keyNameOf(name))
		}
		if _, builtin := builtinProviders[name]; builtin {
			t.Fatalf("%q named as the built-in %s", base, name)
		}
	})
}
