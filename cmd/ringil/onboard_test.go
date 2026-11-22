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

	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/channel/telegram"
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
		"5", base, "y", // another OpenAI-compatible server, which needs a key
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
	h, _ := setupHarness(t, []string{"y", "2", "frase lunga davvero", "frase lunga davvero", "4", "llama3", "", "1", "1", "n"}, map[string]string{})
	h.c.ollama = "http://127.0.0.1:1/v1" // no Ollama there: the list fails, the name is asked
	old := "# my settings\nmodel: old-model\nprofiles:\n  writer: {model: big}  # the writer\n"
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
	for _, want := range []string{"# my settings", "writer:", "# the writer", "base: http://127.0.0.1:1/v1", "model: llama3"} {
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
	h, _ := setupHarness(t, []string{"y", "2", "frase lunga davvero", "frase lunga davvero", "5", base, "y", "good", "1", "", "1", "1", "n", "/exit"},
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
	h, _ := setupHarness(t, []string{"2", "frase lunga davvero", "frase lunga davvero", "5", srv.URL + "/v1", "y", "k", "1", "",
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
	h, _ = setupHarness(t, []string{"y", "2", "frase lunga davvero", "frase lunga davvero", "4", "llama3", "",
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
	h, _ := setupHarness(t, []string{"2", "frase lunga davvero", "frase lunga davvero", "4", "llama3",
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
	h, _ := setupHarness(t, []string{"2", "frase lunga davvero", "frase lunga davvero", "5", srv.URL + "/v1", "n", "m", "", "1", "1", "n"}, map[string]string{})
	if code := h.run("setup"); code != exitOK || strings.Contains(h.err.String(), "API key (not shown") {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	// Plain http to another host: refused, then allowed (allow_http).
	h, _ = setupHarness(t, []string{"2", "frase lunga davvero", "frase lunga davvero", "5",
		"http://ringil-test.invalid:8000/v1", "n", "http://ringil-test.invalid:8000/v1", "y", "n", "m", "", "1", "1", "n"}, map[string]string{})
	h.run("setup")
	data, _ := os.ReadFile(filepath.Join(h.dir, "ringil", "ringil.yaml"))
	if !strings.Contains(string(data), "allow_http: true") || !strings.Contains(h.err.String(), "travel unencrypted") {
		t.Fatalf("%s\n%s", h.err, data)
	}
	// A key the server cannot check (404): asked whether to keep it.
	h, _ = setupHarness(t, []string{"2", "frase lunga davvero", "frase lunga davvero", "5", srv.URL + "/v1", "y", "k", "y", "m", "", "1", "1", "n"}, map[string]string{})
	if code := h.run("setup"); code != exitOK || !strings.Contains(h.err.String(), "could not be checked") {
		t.Fatalf("exit %d: %s", code, h.err)
	}
}

// Audit 22/11: sections were replaced whole, losing what setup does not
// ask (Mem0's headers, a collection prefix, Vault's mount); a second run
// overwrote the backup of the first.
func TestSetupMergesAndKeepsEveryBackup(t *testing.T) {
	answers := []string{"y", "2", "frase lunga davvero", "frase lunga davvero", "4", "llama3", "", "1", "1", "n"}
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
	time.Sleep(1100 * time.Millisecond) // a second run, a second apart
	h2, _ := setupHarness(t, answers, map[string]string{})
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
