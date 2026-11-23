// secrets_move_test.go: moving the keys to another store
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agilira/orpheus/pkg/orpheus"
	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/anthropic"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/store"
)

// fakeVault is a KV version 2 engine at "secret", in memory.
type fakeVault struct {
	mu     sync.Mutex
	values map[string]string
}

func newFakeVault(t *testing.T) (*fakeVault, string) {
	v := &fakeVault{values: map[string]string{}}
	srv := httptest.NewServer(v)
	t.Cleanup(srv.Close)
	return v, srv.URL
}

func (v *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if r.URL.Path == "/v1/sys/internal/ui/mounts/secret" {
		_, _ = w.Write([]byte(`{"data":{"type":"kv","options":{"version":"2"}}}`))
		return
	}
	name, ok := strings.CutPrefix(r.URL.Path, "/v1/secret/data/ringil/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	v.serveEntry(w, r, name)
}

func (v *fakeVault) serveEntry(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodPost:
		var in struct {
			Data map[string]string `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		v.values[name] = in.Data["value"]
		_, _ = w.Write([]byte(`{"data":{"version":1}}`))
	case http.MethodDelete:
		delete(v.values, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		value, found := v.values[name]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[]}`))
			return
		}
		out, _ := json.Marshal(map[string]any{"data": map[string]any{"data": map[string]string{"value": value}, "metadata": map[string]int{"version": 1}}})
		_, _ = w.Write(out)
	}
}

func fileStore(t *testing.T, dir, passphrase string) secrets.Store {
	t.Helper()
	s, err := store.Open(context.Background(), store.Config{Kind: store.File, File: filepath.Join(dir, "ringil", "secrets.enc"), PassphraseEnv: "P"}, secrets.Map{"P": passphrase})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.(interface{ Close() error }).Close() })
	return s
}

// From no store: what the environment holds goes to the file, each read
// back; ringil.yaml switches, keeping the rest; nothing is deleted.
func TestSecretsMoveFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, "# keep me\nprovider: deepseek\nmcp_servers:\n  c: {url: \"https://c.example/mcp\", headers: {X-Key: \"${C_KEY}\"}}\n")
	vars := map[string]string{"DEEPSEEK_API_KEY": "ds", "C_KEY": "c", "UNRELATED": "u", "RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero"}
	out := secretsCommand(t, dir, "", vars, "move", "file")
	if !strings.Contains(out, "copied 2 keys") || !strings.Contains(out, "C_KEY, DEEPSEEK_API_KEY") || strings.Contains(out, "Delete them") {
		t.Fatalf("%s", out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "ringil", "ringil.yaml"))
	if !strings.Contains(string(data), "store: file") || !strings.Contains(string(data), "# keep me") {
		t.Fatalf("%s", data)
	}
	keys := fileStore(t, dir, "frase lunga davvero")
	for name, want := range map[string]string{"DEEPSEEK_API_KEY": "ds", "C_KEY": "c"} {
		if v, _, _ := keys.Lookup(context.Background(), name); v != want {
			t.Errorf("%s: %q", name, v)
		}
	}
	if _, found, _ := keys.Lookup(context.Background(), "UNRELATED"); found {
		t.Error("a variable ringil does not use was copied")
	}
}

// A Vault that cannot be reached switches nothing; one that can gets the
// keys, and the old ones are deleted only on a yes.
func TestSecretsMoveToVault(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, "secrets: {store: file}\n")
	vars := map[string]string{"RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero", "VAULT_TOKEN": "t"}
	secretsCommand(t, dir, "sk-openai\n", vars, "set", "OPENAI_API_KEY")
	h := storeHarness(t, dir, "", vars)
	if code := h.run("secrets", "move", "vault", "--vault-url", "http://127.0.0.1:1", "--vault-path", "ringil"); code == exitOK || !strings.Contains(h.err.String(), "nothing was switched") {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "ringil", "ringil.yaml")); !strings.Contains(string(data), "store: file") {
		t.Fatalf("switched though nothing was copied:\n%s", data)
	}
}

func TestSecretsMoveDeletesOnlyOnAYes(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, "secrets: {store: file}\n")
	vars := map[string]string{"RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero", "VAULT_TOKEN": "t"}
	secretsCommand(t, dir, "sk-openai\n", vars, "set", "OPENAI_API_KEY")
	vault, url := newFakeVault(t)
	writeProfiles(t, dir, "secrets: {store: file, vault: {allow_http: true}}\n")
	out := secretsCommand(t, dir, "y\n", vars, "move", "vault", "--vault-url", url, "--vault-path", "ringil")
	if !strings.Contains(out, "copied 1 keys") || !strings.Contains(out, "deleted from the file store") || vault.values["OPENAI_API_KEY"] != "sk-openai" {
		t.Fatalf("%s %v", out, vault.values)
	}
	if _, found, _ := fileStore(t, dir, "frase lunga davvero").Lookup(context.Background(), "OPENAI_API_KEY"); found {
		t.Fatal("the old key stayed after a yes")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "ringil", "ringil.yaml"))
	if !strings.Contains(string(data), "store: vault") || !strings.Contains(string(data), url) || !strings.Contains(string(data), "path: ringil") {
		t.Fatalf("%s", data)
	}
}

func TestSecretsMoveRefused(t *testing.T) {
	if _, err := movedTo(secretsConfig{Store: "file"}, "file", moveFlags{}); err == nil {
		t.Error("a move to the same store")
	}
	if _, err := movedTo(secretsConfig{}, "vault", moveFlags{}); err == nil {
		t.Error("Vault without its address")
	}
	names := (&config{Providers: map[string]providerEntry{"lan": {Base: "https://lan/v1", KeyEnv: "LAN_KEY"}}}).secretNames()
	for _, want := range []string{"LAN_KEY", "OPENAI_API_KEY", "RINGIL_TELEGRAM_TOKEN", chatgptLogin, chatgptLogin + "_ACCESS"} {
		if !slices.Contains(names, want) {
			t.Errorf("%s not among %v", want, names)
		}
	}
	for _, never := range []string{"RINGIL_SECRETS_PASSPHRASE", "VAULT_TOKEN", ""} {
		if slices.Contains(names, never) {
			t.Errorf("%q among the keys to move", never)
		}
	}
}

// Audit 23/11: the configuration reloads while the question waits, and
// the yes deleted the keys from the NEW store. The old one, opened before
// the switch, is the one emptied.
func TestSecretsMoveDeletesFromTheOldStoreAfterAReload(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, "secrets: {store: file, vault: {allow_http: true}}\n")
	vars := map[string]string{"RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero", "VAULT_TOKEN": "t"}
	secretsCommand(t, dir, "sk-openai\n", vars, "set", "OPENAI_API_KEY")
	vault, url := newFakeVault(t)
	r, w := io.Pipe()
	h := storeHarness(t, dir, "", vars)
	h.c.in = r
	h.c.configEvery = 10 * time.Millisecond
	go func() {
		time.Sleep(300 * time.Millisecond) // a person reading the question: the file has reloaded
		_, _ = w.Write([]byte("y\n"))
	}()
	if code := h.run("secrets", "move", "vault", "--vault-url", url, "--vault-path", "ringil"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if vault.values["OPENAI_API_KEY"] != "sk-openai" {
		t.Fatalf("the new store lost the key: %v", vault.values)
	}
	if _, found, _ := fileStore(t, dir, "frase lunga davvero").Lookup(context.Background(), "OPENAI_API_KEY"); found {
		t.Fatal("the old store kept it")
	}
}

// For a container: the keys in a new env file, read back; ringil.yaml
// names no store; an existing file is refused.
func TestSecretsMoveToAnEnvFile(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, "secrets: {store: file}\n")
	vars := map[string]string{"RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero"}
	secretsCommand(t, dir, "-----BEGIN-----\nabc\n-----END-----\n", vars, "set", "ANTHROPIC_API_KEY")
	path := filepath.Join(t.TempDir(), "keys.env")
	out := secretsCommand(t, dir, "n\n", vars, "move", "env", "--to-file", path)
	if !strings.Contains(out, "wrote 1 keys to "+path) || !strings.Contains(out, "--env-file") {
		t.Fatalf("%s", out)
	}
	res, err := secrets.EnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := res.Lookup(context.Background(), "ANTHROPIC_API_KEY"); v != "-----BEGIN-----\nabc\n-----END-----" {
		t.Fatalf("%q", v)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "ringil", "ringil.yaml")); strings.Contains(string(data), "store:") {
		t.Fatalf("still names a store:\n%s", data)
	}
	writeProfiles(t, dir, "secrets: {store: file}\n")
	h := storeHarness(t, dir, "", vars)
	if code := h.run("secrets", "move", "env", "--to-file", path); code == exitOK {
		t.Fatal("an existing env file was overwritten")
	}
}

// Audit 23/11: an MCP server's OAuth secret is moved too; what opens a
// store never is, even when the file names it; a reference too long to be
// a name does not stop the move.
func TestSecretNamesFromTheFile(t *testing.T) {
	f := &config{
		Secrets: secretsConfig{PassphraseEnv: "MY_PASS", Vault: vaultConfig{TokenEnv: "MY_VAULT"}},
		MCPServers: map[string]mcpEntry{"gh": {URL: "https://gh.example/mcp", OAuth: &oauthEntry{ClientSecret: "${GH_OAUTH_SECRET}"},
			Headers: map[string]string{"X": "${MY_PASS}${VAULT_TOKEN}${" + strings.Repeat("A", 200) + "}"}}},
	}
	names := f.secretNames()
	if !slices.Contains(names, "GH_OAUTH_SECRET") {
		t.Errorf("the OAuth secret is not moved: %v", names)
	}
	for _, never := range []string{"MY_PASS", "MY_VAULT", "VAULT_TOKEN", strings.Repeat("A", 200)} {
		if slices.Contains(names, never) {
			t.Errorf("%.20s among the keys to move", never)
		}
	}
	if _, err := movedTo(secretsConfig{Store: "file"}, "env", moveFlags{}); err == nil {
		t.Error("a move to env without its file")
	}
	if _, err := movedTo(secretsConfig{}, "env", moveFlags{file: "/x"}); err == nil {
		t.Error("a move to env from no store")
	}
}

// Audit 23/11: a ChatGPT sign-in needs a store; an env file would hold a
// copy never read.
func TestSecretsMoveKeepsTheSignInInAStore(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, "secrets: {store: file}\n")
	vars := map[string]string{"RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero"}
	secretsCommand(t, dir, "{}\n", vars, "set", chatgptLogin)
	path := filepath.Join(t.TempDir(), "keys.env")
	h := storeHarness(t, dir, "", vars)
	if code := h.run("secrets", "move", "env", "--to-file", path); code == exitOK || !strings.Contains(h.err.String(), "needs a store") {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the env file was written")
	}
}

// 23/11: after a move, the keys still in the environment are named (a
// store refuses a key in both places), and where each key of a run came
// from is in the audit's hash chain, by name.
func TestShadowsNamedAndSourcesAudited(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, "provider: openai\nmodel: m\n")
	vars := map[string]string{"OPENAI_API_KEY": "sk-env", "RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero"}
	out := secretsCommand(t, dir, "", vars, "move", "file")
	if !strings.Contains(out, "still in your environment or env file, remove them") || !strings.Contains(out, "OPENAI_API_KEY") || strings.Contains(out, "sk-env") {
		t.Fatalf("%s", out)
	}
	delete(vars, "OPENAI_API_KEY")
	if k := keyOfRun(t, dir, vars); k != "sk-env" {
		t.Fatalf("key %q", k)
	}
	records := auditRecords(t, filepath.Join(dir, "ringil", "audit.db"))
	if !strings.Contains(records, "ringil_key_source") || !strings.Contains(records, `"name":"OPENAI_API_KEY"`) || !strings.Contains(records, `"source":"store"`) || strings.Contains(records, "sk-env") {
		t.Fatalf("%s", records)
	}
}

// Audit 23/11: with a store, a provider's key comes from it alone. A key
// planted in the environment for a provider the store has no key for, the
// file's provider having none in the store, is refused: the conversations
// would go to the planter's account.
func TestAPlantedProviderKeyIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, "secrets: {store: file}\nprovider: anthropic\nmodel: m\n")
	vars := map[string]string{"RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero"}
	secretsCommand(t, dir, "sk-mine\n", vars, "set", "OPENAI_API_KEY")
	planted := map[string]string{"RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero", "ANTHROPIC_API_KEY": "sk-theirs"}
	h := storeHarness(t, dir, "", planted)
	h.c.newAnthropic = func(cfg anthropic.Config) (agent.Provider, error) {
		_, err := cfg.KeyFunc(context.Background())
		return nil, err
	}
	if code := h.run("--no-tools", "run", "hi"); code == exitOK || !strings.Contains(h.err.String(), "ANTHROPIC_API_KEY is in the environment (or the env file) but not in the secrets store") || strings.Contains(h.err.String(), "sk-theirs") {
		t.Fatalf("exit %d: %s", code, h.err)
	}
}

// Audit 23/11: from the environment, an MCP server's env passes settings
// through (${PROJECT_DIR}): they stay there, named; credentials move.
func TestMoveLeavesSettingsInTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, "mcp_servers:\n  f: {command: mcp-files, env: {ROOT: \"${PROJECT_DIR}\", TOKEN: \"${FILES_TOKEN}\"}, headers: {}}\n")
	vars := map[string]string{"PROJECT_DIR": "/srv", "FILES_TOKEN": "t", "RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero"}
	out := secretsCommand(t, dir, "", vars, "move", "file")
	if !strings.Contains(out, "may be settings (MCP servers' env): FILES_TOKEN, PROJECT_DIR") {
		t.Fatalf("%s", out)
	}
	if _, found, _ := fileStore(t, dir, "frase lunga davvero").Lookup(context.Background(), "PROJECT_DIR"); found {
		t.Fatal("a setting was moved into the store")
	}
}

// Audit 23/11: a key just set that the environment has too is named.
func TestSecretsSetNamesAShadow(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, "secrets: {store: file}\n")
	vars := map[string]string{"RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero", "OPENAI_API_KEY": "old"}
	if out := secretsCommand(t, dir, "new\n", vars, "set", "OPENAI_API_KEY"); !strings.Contains(out, "OPENAI_API_KEY is in your environment or env file too") {
		t.Fatalf("%s", out)
	}
}

// Audit 23/11: with a store, a provider named in the environment is
// refused (a keyless one, Ollama's port, would take the conversations);
// serve's and the bot's tokens come from the store alone.
func TestWithAStoreTheEnvironmentPicksNoProviderNorToken(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, "secrets: {store: file}\nprovider: openai\nmodel: m\n")
	vars := map[string]string{"RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero"}
	secretsCommand(t, dir, "sk-mine\n", vars, "set", "OPENAI_API_KEY")
	planted := map[string]string{"RINGIL_SECRETS_PASSPHRASE": "frase lunga davvero", "RINGIL_PROVIDER": "ollama", "RINGIL_SERVE_TOKEN": "planted-token"}
	h := storeHarness(t, dir, "", planted)
	if code := h.run("--no-tools", "run", "hi"); code == exitOK || !strings.Contains(h.err.String(), "RINGIL_PROVIDER is in the environment") {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	h = storeHarness(t, dir, "", planted)
	probe(t, h.c, nil, func(ctx *orpheus.Context) error {
		defer h.c.shutdown()
		if err := h.c.loadConfig(ctx); err != nil {
			return err
		}
		if tok, _, err := h.c.serveToken(ctx); err == nil || tok == "planted-token" {
			t.Errorf("serve took the planted token: %v", err)
		}
		return nil
	})
	h = storeHarness(t, dir, "", planted, fake.Reply("ok"))
	if code := h.run("--provider", "openai", "--no-tools", "run", "hi"); code != exitOK {
		t.Fatalf("the command line's provider: exit %d: %s", code, h.err)
	}
}
