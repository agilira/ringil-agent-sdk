// onboard_test.go: the guided setup, answer by answer
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/channel/telegram"
	"github.com/agilira/ringil-agent-sdk/mcp"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/store"
)

// modelServer lists two models for the key "good", refuses any other.
func modelServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"model-a"},{"id":"model-b"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// fakeBot answers getMe as @ringil_test_bot; its chat brings a group
// message first (ignored), then the owner's /start.
type fakeBot struct {
	in   []channel.Message
	sent []string
}

func (b *fakeBot) Name() string     { return "telegram" }
func (b *fakeBot) Username() string { return "ringil_test_bot" }
func (b *fakeBot) Receive(ctx context.Context) (channel.Message, error) {
	if len(b.in) == 0 {
		<-ctx.Done()
		return channel.Message{}, ctx.Err()
	}
	m := b.in[0]
	b.in = b.in[1:]
	return m, nil
}
func (b *fakeBot) Send(_ context.Context, chat string, out channel.Out) error {
	b.sent = append(b.sent, chat+": "+out.Text)
	return nil
}

func setupHarness(t *testing.T, answers []string, vars map[string]string) (*harness, *fakeBot) {
	t.Helper()
	h := newHarness(t, strings.Join(answers, "\n")+"\n", fake.Reply("ready"))
	env(h, vars)
	bot := &fakeBot{in: []channel.Message{{Chat: "g", From: "7", Group: true, Text: "/start"}, {Chat: "42", From: "42", Text: "/start"}}}
	h.c.newTelegram = func(context.Context, telegram.Config) (telegramBot, error) { return bot, nil }
	return h, bot
}

// 22/11: the first start asks only what it needs, keeps every key in the
// store chosen first, finds the bot's owner by their /start, and writes
// ringil.yaml.
func TestSetupFromNothing(t *testing.T) {
	base := modelServer(t)
	prompt := filepath.Join(t.TempDir(), "me.md")
	if err := os.WriteFile(prompt, []byte("Rispondi in italiano."), 0o600); err != nil {
		t.Fatal(err)
	}
	h, bot := setupHarness(t, []string{
		"2", "frase lunga davvero", "frase lunga davvero", // an encrypted file
		"6", base, "y", // another OpenAI-compatible server, which needs a key
		"wrong", "good", // a refused key, then the right one
		"1",                   // the first model listed (newest name first: model-b)
		prompt, "1", "1", "y", // the system prompt, no embedding model, SQLite, a Telegram bot
		"123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	}, map[string]string{})
	if code := h.run("setup"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	checkWritten(t, h.dir, base, prompt)
	if len(bot.sent) != 1 || !strings.HasPrefix(bot.sent[0], "42: ") {
		t.Fatalf("the owner was not told: %v", bot.sent)
	}
	keys, err := store.Open(context.Background(), store.Config{Kind: store.File, File: filepath.Join(h.dir, "ringil", "secrets.enc"), PassphraseEnv: "P"}, secrets.Map{"P": "frase lunga davvero"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keys.(interface{ Close() error }).Close() })
	for name, want := range map[string]string{"RINGIL_API_KEY": "good", "RINGIL_TELEGRAM_TOKEN": "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ"} {
		if v, _, err := keys.Lookup(context.Background(), name); err != nil || v != want {
			t.Errorf("%s: %q %v", name, v, err)
		}
	}
}

// checkWritten: ringil.yaml holds the answers, and no key.
func checkWritten(t *testing.T, dir, base, prompt string) {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "ringil", "ringil.yaml"))
	for _, want := range []string{"provider: openai", "base: " + base, "model: model-a", "instructions: " + prompt, "kind: sqlite", "store: file", "- \"42\""} {
		if !strings.Contains(string(data), want) {
			t.Errorf("ringil.yaml lacks %q:\n%s", want, data)
		}
	}
	if strings.Contains(string(data), "good") || strings.Contains(string(data), "ABCDEF") {
		t.Fatalf("a key in ringil.yaml:\n%s", data)
	}
}

// Run again, it updates what it asks and keeps the rest of the file
// (profiles, comments), the old one beside it.
func TestSetupKeepsTheRest(t *testing.T) {
	h, _ := setupHarness(t, []string{"y", "2", "frase lunga davvero", "frase lunga davvero", "5", "llama3", "", "1", "1", "n"}, map[string]string{})
	h.c.ollama = "http://127.0.0.1:1/v1" // no Ollama there: the list fails, the name is asked
	old := "# my settings\nmodel: old-model\nprofiles:\n  writer: {model: big}  # the writer\nnotes: https://example.com/a/very/long/address/of/the/team/notes/that/goes/past/eighty/columns\n"
	writeProfiles(t, h.dir, old)
	if code := h.run("setup"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	data, _ := os.ReadFile(filepath.Join(h.dir, "ringil", "ringil.yaml"))
	baks, _ := filepath.Glob(filepath.Join(h.dir, "ringil", "ringil.yaml.bak-*"))
	var bak []byte
	if len(baks) == 1 {
		bak, _ = os.ReadFile(baks[0])
	}
	for _, want := range []string{"# my settings", "writer:", "# the writer", "base: http://127.0.0.1:1/v1", "model: llama3", "notes: https://example.com/a/very/long/address/of/the/team/notes/that/goes/past/eighty/columns\n"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("lost %q:\n%s", want, data)
		}
	}
	if string(bak) != old {
		t.Fatalf("the old file was not kept: %q", bak)
	}
}

// A person who starts ringil with nothing set up is offered the setup,
// then the chat starts with what it wrote.
func TestFirstRunOffersTheSetup(t *testing.T) {
	base := modelServer(t)
	h, _ := setupHarness(t, []string{"y", "2", "frase lunga davvero", "frase lunga davvero", "6", base, "y", "good", "1", "", "1", "1", "n", "n", "/exit"},
		map[string]string{}) // the passphrase is not in the environment: the chat goes on with the setup's (audit 22/11)
	h.c.interactive = func() bool { return true }
	if code := h.run(); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if out := h.err.String(); !strings.Contains(out, "not set up yet") || !strings.Contains(out, "set RINGIL_SECRETS_PASSPHRASE") {
		t.Fatalf("%s", out)
	}
	if h.config.Model != "model-a" {
		t.Fatalf("the chat did not start with the new settings: %+v", h.config)
	}
}

// The embedding model is optional: at the chat model's server (its
// models whose name says embedding), or at Ollama, the only way with a
// provider without embedding models; Chroma is offered only then.
func TestSetupEmbeddingModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"chat-model"},{"id":"text-embedding-3-small"}]}`)
	}))
	t.Cleanup(srv.Close)
	h, _ := setupHarness(t, []string{"2", "frase lunga davvero", "frase lunga davvero", "6", srv.URL + "/v1", "y", "k", "1", "",
		"3", "1", // a model of the same server: the one listed with "embed"
		"3", "http://127.0.0.1:8000", "n"}, map[string]string{}) // Chroma, now offered
	if code := h.run("setup"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	data, _ := os.ReadFile(filepath.Join(h.dir, "ringil", "ringil.yaml"))
	for _, want := range []string{"embed_model: text-embedding-3-small", "kind: chroma"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("lacks %q:\n%s", want, data)
		}
	}
	h, _ = setupHarness(t, []string{"y", "2", "frase lunga davvero", "frase lunga davvero", "5", "llama3", "",
		"2", "", "", "1", "n"}, map[string]string{}) // Ollama, its defaults
	h.c.ollama = "http://127.0.0.1:1/v1"
	if code := h.run("setup"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	data, _ = os.ReadFile(filepath.Join(h.dir, "ringil", "ringil.yaml"))
	if !strings.Contains(string(data), "model: qwen3-embedding:0.6b") || !strings.Contains(string(data), "kind: ollama") {
		t.Fatalf("no Ollama embedder:\n%s", data)
	}
}

// Paths the user types go through argus's ValidateSecurePath: a system
// file or a key directory is no system prompt.
func TestSetupRefusesUnsafePaths(t *testing.T) {
	h, _ := setupHarness(t, []string{"2", "frase lunga davvero", "frase lunga davvero", "5", "llama3",
		"/etc/passwd", "~/.ssh/id_rsa", "../../etc/hosts"}, map[string]string{})
	h.c.ollama = "http://127.0.0.1:1/v1"
	if code := h.run("setup"); code == exitOK || strings.Count(h.err.String(), "not a path ringil reads") != 3 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
}

// Audit 22/11: a keyless server could not be set up; plain http to
// another machine carried the key without asking, then failed in the CLI;
// a key the server could not check was kept without a word.
func TestSetupServersAndKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(srv.Close)
	// No key: none is asked, none kept.
	h, _ := setupHarness(t, []string{"2", "frase lunga davvero", "frase lunga davvero", "6", srv.URL + "/v1", "n", "m", "", "1", "1", "n"}, map[string]string{})
	if code := h.run("setup"); code != exitOK || strings.Contains(h.err.String(), "API key (not shown") {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	// Plain http to another host: refused, then allowed (allow_http).
	h, _ = setupHarness(t, []string{"2", "frase lunga davvero", "frase lunga davvero", "6",
		"http://ringil-test.invalid:8000/v1", "n", "http://ringil-test.invalid:8000/v1", "y", "n", "m", "", "1", "1", "n"}, map[string]string{})
	h.run("setup")
	data, _ := os.ReadFile(filepath.Join(h.dir, "ringil", "ringil.yaml"))
	if !strings.Contains(string(data), "allow_http: true") || !strings.Contains(h.err.String(), "travel unencrypted") {
		t.Fatalf("%s\n%s", h.err, data)
	}
	// A key the server cannot check (404): asked whether to keep it.
	h, _ = setupHarness(t, []string{"2", "frase lunga davvero", "frase lunga davvero", "6", srv.URL + "/v1", "y", "k", "y", "m", "", "1", "1", "n"}, map[string]string{})
	if code := h.run("setup"); code != exitOK || !strings.Contains(h.err.String(), "could not be checked") {
		t.Fatalf("exit %d: %s", code, h.err)
	}
}

// Audit 22/11: sections were replaced whole, losing what setup does not
// ask (Mem0's headers, a collection prefix, Vault's mount); a second run
// overwrote the backup of the first.
func TestSetupMergesAndKeepsEveryBackup(t *testing.T) {
	answers := []string{"y", "2", "frase lunga davvero", "frase lunga davvero", "5", "llama3", "", "1", "1", "n"}
	h, _ := setupHarness(t, answers, map[string]string{})
	h.c.ollama = "http://127.0.0.1:1/v1"
	writeProfiles(t, h.dir, "memory_store:\n  kind: mem0\n  url: http://m:8888\n  collection_prefix: acme\n  headers: {X-API-Key: x}\nsecrets:\n  store: vault\n  vault: {url: https://v, path: p, mount: kv}\n")
	if code := h.run("setup"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	data, _ := os.ReadFile(filepath.Join(h.dir, "ringil", "ringil.yaml"))
	for _, want := range []string{"collection_prefix: acme", "X-API-Key", "mount: kv", "kind: sqlite", "store: file"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("lacks %q:\n%s", want, data)
		}
	}
	if strings.Contains(string(data), "url: http://m:8888") {
		t.Errorf("the Mem0 url stayed with SQLite:\n%s", data)
	}
	time.Sleep(1100 * time.Millisecond)                                                                                                         // a second run, a second apart
	h2, _ := setupHarness(t, append(append([]string(nil), answers[:5]...), append([]string{"llama4"}, answers[6:]...)...), map[string]string{}) // another model: a change, so a write and a backup
	h2.c.configDir, h2.dir, h2.c.ollama = h.c.configDir, h.dir, "http://127.0.0.1:1/v1"
	if code := h2.run("setup"); code != exitOK {
		t.Fatalf("second run: %s", h2.err)
	}
	if baks, _ := filepath.Glob(filepath.Join(h.dir, "ringil", "ringil.yaml.bak-*")); len(baks) != 2 {
		t.Fatalf("backups %v", baks)
	}
}

// Models newest first by their dates, and only chat models offered as the
// chat model (audit 22/11: whisper, tts and embeddings came first).
func TestModelOrder(t *testing.T) {
	ids := modelIDs([]listedModel{{ID: "old", Created: 1}, {ID: "new", Created: 3}, {ID: "anth", CreatedAt: "2026-11-01T00:00:00Z"}})
	if strings.Join(ids, " ") != "anth new old" {
		t.Fatalf("%v", ids)
	}
	if got := chatModels([]string{"gpt-6", "whisper-1", "tts-1", "text-embedding-3-small", "dall-e-3", "gpt-6-mini"}); strings.Join(got, " ") != "gpt-6 gpt-6-mini" {
		t.Fatalf("%v", got)
	}
}

// 22/11: Composio's consumer key is tried, a refused one asked again, and
// kept in the store; ringil.yaml names it, and a browser sign-in left from
// before gives way to the key.
func TestSetupComposio(t *testing.T) {
	answers := []string{"y", "2", "frase lunga davvero", "frase lunga davvero", "5", "llama3", "", "1", "1", "n"}
	h, _ := setupHarness(t, append(answers, "y", "ck_wrong", "ck_right"), map[string]string{})
	h.c.ollama = "http://127.0.0.1:1/v1"
	var tried []string
	h.c.checkComposio = func(_ context.Context, key string) (int, error) {
		tried = append(tried, key)
		if key != "ck_right" {
			return 0, errors.New(mcp.ErrRefused, "refused")
		}
		return 12, nil
	}
	writeProfiles(t, h.dir, "mcp_servers:\n  apps:\n    command: npx\n    args: [composio]\n    env: {K: v}\n    url: https://Connect.Composio.dev/mcp\n    oauth: {}\n    headers: {X-Consumer-Api-Key: ck_old, X-Other: keep}\n    tools: [COMPOSIO_SEARCH_TOOLS]\n    disabled: true\n")
	token := filepath.Join(h.dir, "ringil", "oauth", "apps.json")
	if err := os.MkdirAll(filepath.Dir(token), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run("setup"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	// The entry already at Composio is updated, under its own name.
	yamlHas(t, h.dir, []string{"apps:", "x-consumer-api-key", "${COMPOSIO_KEY}", "url: https://connect.composio.dev/mcp", "COMPOSIO_SEARCH_TOOLS", "X-Other: keep"},
		[]string{"oauth", "disabled", "ck_", "X-Consumer-Api-Key", "command", "npx", "env", "composio:"})
	if _, err := os.Stat(token); !os.IsNotExist(err) {
		t.Errorf("the old sign-in's token stayed: %v", err)
	}
	if len(tried) != 2 || !strings.Contains(h.out.String()+h.err.String(), "12 tools") {
		t.Fatalf("tried %v, said %s", tried, h.out)
	}
	if v := storedSecret(t, h.dir, composioKey); v != "ck_right" {
		t.Fatalf("the key in the store: %q", v)
	}
}

// Composio out of reach: the key is kept when the user says so.
func TestSetupComposioUnreachable(t *testing.T) {
	answers := []string{"2", "frase lunga davvero", "frase lunga davvero", "5", "llama3", "", "1", "1", "n"}
	h2, _ := setupHarness(t, append(answers, "y", "ck_later", "y"), map[string]string{})
	h2.c.ollama = "http://127.0.0.1:1/v1"
	h2.c.checkComposio = func(context.Context, string) (int, error) { return 0, errors.New(mcp.ErrConnect, "no network") }
	if code := h2.run("setup"); code != exitOK {
		t.Fatalf("unreachable: %s", h2.err)
	}
	if data, _ := os.ReadFile(filepath.Join(h2.dir, "ringil", "ringil.yaml")); !strings.Contains(string(data), "${COMPOSIO_KEY}") {
		t.Fatalf("unreachable, kept:\n%s", data)
	}
}

// storedSecret reads a secret back from the setup's encrypted file.
func storedSecret(t *testing.T, dir, name string) string {
	t.Helper()
	keys, err := store.Open(context.Background(), store.Config{Kind: store.File, File: filepath.Join(dir, "ringil", "secrets.enc"), PassphraseEnv: "P"}, secrets.Map{"P": "frase lunga davvero"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = keys.(interface{ Close() error }).Close() }()
	v, _, err := keys.Lookup(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// yamlHas: ringil.yaml holds every want and none of not.
func yamlHas(t *testing.T, dir string, want, not []string) {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "ringil", "ringil.yaml"))
	for _, w := range want {
		if !strings.Contains(string(data), w) {
			t.Errorf("lacks %q:\n%s", w, data)
		}
	}
	for _, n := range not {
		if strings.Contains(string(data), n) {
			t.Errorf("holds %q:\n%s", n, data)
		}
	}
}

// Audit 22/11: Composio is optional, so giving up on it does not stop the
// setup: three refused keys, or a key not kept, skip it.
func TestSetupComposioSkipped(t *testing.T) {
	answers := []string{"2", "frase lunga davvero", "frase lunga davvero", "5", "llama3", "", "1", "1", "n"}
	for name, more := range map[string][]string{
		"refused thrice": {"y", "ck_a", "ck_b", "ck_c"},
		"not kept":       {"y", "ck_a", "n"},
	} {
		h, _ := setupHarness(t, append(answers, more...), map[string]string{})
		h.c.ollama = "http://127.0.0.1:1/v1"
		h.c.checkComposio = func(context.Context, string) (int, error) {
			if name == "not kept" {
				return 0, errors.New(mcp.ErrConnect, "no network")
			}
			return 0, errors.New(mcp.ErrRefused, "refused")
		}
		if code := h.run("setup"); code != exitOK {
			t.Fatalf("%s: exit %d: %s", name, code, h.err)
		}
		yamlHas(t, h.dir, []string{"model: llama3"}, []string{"mcp_servers", "COMPOSIO"})
	}
}

// Audit 22/11 (setup on argus's writer): owners written as numbers, as the
// loader accepts them, were dropped; a key to remove that is an alias
// stayed, the run then using the old value: it is refused, the file left
// as it was.
func TestSetupKeepsNumericOwnersAndRefusesAliases(t *testing.T) {
	answers := []string{"y", "2", "frase lunga davvero", "frase lunga davvero", "5", "llama3", "", "1", "1", "y", "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ"}
	h, _ := setupHarness(t, answers, map[string]string{})
	h.c.ollama = "http://127.0.0.1:1/v1"
	writeProfiles(t, h.dir, "serve:\n  telegram:\n    owners: [111, 222]\n")
	if code := h.run("setup"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	yamlHas(t, h.dir, []string{"111", "222", "\"42\""}, nil)

	h2, _ := setupHarness(t, append(append([]string(nil), answers[:9]...), "n"), map[string]string{})
	h2.c.ollama = "http://127.0.0.1:1/v1"
	const aliased = "x: &b http://old.example/v1\nbase: *b\n"
	writeProfiles(t, h2.dir, aliased)
	if code := h2.run("setup"); code == exitOK || !strings.Contains(h2.err.String(), "alias") {
		t.Fatalf("an alias left: exit %d: %s", code, h2.err)
	}
	if data, _ := os.ReadFile(filepath.Join(h2.dir, "ringil", "ringil.yaml")); string(data) != aliased {
		t.Fatalf("the file changed: %q", data)
	}
}

func TestSetupFailedWriteKeepsOAuthToken(t *testing.T) {
	answers := []string{"y", "2", "frase lunga davvero", "frase lunga davvero", "5", "llama3", "", "1", "1", "n", "y", "ck_right"}
	h, _ := setupHarness(t, answers, map[string]string{})
	h.c.ollama = "http://127.0.0.1:1/v1"
	h.c.checkComposio = func(context.Context, string) (int, error) { return 4, nil }
	const aliased = "x: &b http://old.example/v1\nbase: *b\nmcp_servers:\n  composio:\n    url: https://connect.composio.dev/mcp\n    oauth: {}\n"
	writeProfiles(t, h.dir, aliased)
	token := filepath.Join(h.dir, "ringil", "oauth", "composio.json")
	if err := os.MkdirAll(filepath.Dir(token), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run("setup"); code == exitOK {
		t.Fatalf("expected setup to fail on alias")
	}
	if _, err := os.Stat(token); err != nil {
		t.Fatalf("oauth token deleted even though ringil.yaml did not write: %v", err)
	}
}
