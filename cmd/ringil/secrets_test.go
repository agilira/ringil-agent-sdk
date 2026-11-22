// secrets_test.go: keys kept in the store of ringil.yaml
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/store"
)

// storeHarness makes a harness on dir (its configuration) with these
// variables.
func storeHarness(t *testing.T, dir, input string, vars map[string]string, steps ...fake.Step) *harness {
	t.Helper()
	h := newHarness(t, input, steps...)
	h.dir = dir
	h.c.configDir = func() (string, error) { return dir, nil }
	env(h, vars)
	return h
}

// keyOfRun is the key a run on dir sends, with vars.
func keyOfRun(t *testing.T, dir string, vars map[string]string) string {
	t.Helper()
	h := storeHarness(t, dir, "", vars, fake.Reply("ok"))
	key := "unread"
	h.c.newProvider = func(cfg openai.Config) (agent.Provider, error) {
		// Read while the run lasts: the stores close with it.
		key = ""
		if cfg.KeyFunc != nil {
			k, err := cfg.KeyFunc(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			key = k
		}
		return h.script, nil
	}
	if code := h.run("--no-tools", "run", "hi"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	return key
}

// secretsCommand runs a secrets command on dir, which must succeed.
func secretsCommand(t *testing.T, dir, input string, vars map[string]string, args ...string) string {
	t.Helper()
	h := storeHarness(t, dir, input, vars)
	if code := h.run(append([]string{"secrets"}, args...)...); code != exitOK {
		t.Fatalf("%v: exit %d: %s", args, code, h.err)
	}
	return h.out.String() + h.err.String()
}

// 22/11: the keys came from the environment and the --env-file alone; the
// store of ringil.yaml serves every part of ringil, the environment
// winning for one run.
func TestSecretsInTheStore(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, `{"secrets":{"store":"file"},"key_env":"MY_KEY"}`)
	vars := map[string]string{"RINGIL_BASE_URL": "http://localhost:1/v1", "RINGIL_MODEL": "m", "RINGIL_SECRETS_PASSPHRASE": "frase lunga"}
	if out := secretsCommand(t, dir, "sk-from-store\n", vars, "set", "MY_KEY"); !strings.Contains(out, "saved MY_KEY in the file store") {
		t.Fatalf("%s", out)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "ringil", "secrets.enc")); err != nil || strings.Contains(string(data), "sk-from-store") {
		t.Fatalf("the file: %v", err)
	}
	if k := keyOfRun(t, dir, vars); k != "sk-from-store" {
		t.Fatalf("key %q", k)
	}
	over := map[string]string{"MY_KEY": "from-env"}
	for k, v := range vars {
		over[k] = v
	}
	if k := keyOfRun(t, dir, over); k != "from-env" {
		t.Fatalf("the environment does not win: %q", k)
	}
	secretsCommand(t, dir, "", vars, "rm", "MY_KEY")
	if k := keyOfRun(t, dir, vars); k != "" {
		t.Fatalf("still there after rm: %q", k)
	}
}

func TestSecretsCommandsChecked(t *testing.T) {
	dir := t.TempDir()
	h := storeHarness(t, dir, "v\n", map[string]string{})
	if code := h.run("secrets", "set", "MY_KEY"); code == exitOK || !strings.Contains(h.err.String(), "set secrets.store") {
		t.Fatalf("no store: exit %d %s", code, h.err)
	}
	writeProfiles(t, dir, `{"secrets":{"store":"file"}}`)
	for _, args := range [][]string{{"secrets", "set"}, {"secrets", "set", "bad-name"}, {"secrets", "set", "A", "B"}} {
		h = storeHarness(t, dir, "v\n", map[string]string{"RINGIL_SECRETS_PASSPHRASE": "p"})
		if code := h.run(args...); code == exitOK {
			t.Errorf("%v accepted", args)
		}
	}
	h = storeHarness(t, dir, "v\n", map[string]string{}) // no passphrase
	if code := h.run("secrets", "set", "MY_KEY"); code == exitOK || !strings.Contains(h.err.String(), "RINGIL_SECRETS_PASSPHRASE") {
		t.Fatalf("no passphrase: exit %d %s", code, h.err)
	}
}

func TestSecretsSectionChecked(t *testing.T) {
	dir := t.TempDir()
	for body, want := range map[string]string{
		`{"secrets":{"store":"safe"}}`:                        "keyring, vault or file",
		`{"secrets":{"store":"vault"}}`:                       "Vault needs its URL",
		`{"secrets":{"store":"file","file":"rel/s.enc"}}`:     "absolute path",
		`{"secrets":{"store":"file","passphrase_env":"a-b"}}`: "secrets:",
	} {
		writeProfiles(t, dir, body)
		h := storeHarness(t, dir, "", map[string]string{"RINGIL_BASE_URL": "http://localhost:1/v1", "RINGIL_MODEL": "m"}, fake.Reply("ok"))
		if code := h.run("--no-tools", "run", "hi"); code == exitOK || !strings.Contains(h.err.String(), want) {
			t.Errorf("%s: exit %d %s", body, code, h.err)
		}
	}
}

// Audit 22/11: every setting absent from the environment went to the
// store: an entry there changed ringil.yaml's, and a store that could not
// answer stopped every run. Settings are the environment's, the
// --env-file's and ringil.yaml's alone.
func TestSettingsAreNotSecrets(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, `{"secrets":{"store":"file"},"model":"from-yaml"}`)
	vars := map[string]string{"RINGIL_BASE_URL": "http://localhost:1/v1", "RINGIL_SECRETS_PASSPHRASE": "p", "RINGIL_API_KEY": "k"}
	secretsCommand(t, dir, "planted-model\n", vars, "set", "RINGIL_MODEL")
	h := storeHarness(t, dir, "", vars, fake.Reply("ok"))
	if code := h.run("--no-tools", "run", "hi"); code != exitOK || h.config.Model != "from-yaml" {
		t.Fatalf("exit %d, model %q: %s", code, h.config.Model, h.err)
	}
	// A Vault no one answers at: the run, its key in the environment,
	// goes on.
	writeProfiles(t, dir, `{"secrets":{"store":"vault","vault":{"url":"http://127.0.0.1:1","path":"x"}}}`)
	h = storeHarness(t, dir, "", map[string]string{"RINGIL_BASE_URL": "http://localhost:1/v1", "RINGIL_MODEL": "m", "RINGIL_API_KEY": "k"}, fake.Reply("ok"))
	if code := h.run("--no-tools", "run", "hi"); code != exitOK {
		t.Fatalf("a store that cannot answer stopped the run: %s", h.err)
	}
}

// From a pipe the whole value (a PEM key spans lines), less one final
// line break; an empty one is refused.
func TestSecretsSetValues(t *testing.T) {
	dir := t.TempDir()
	writeProfiles(t, dir, `{"secrets":{"store":"file"},"key_env":"MY_KEY"}`)
	vars := map[string]string{"RINGIL_BASE_URL": "http://localhost:1/v1", "RINGIL_MODEL": "m", "RINGIL_SECRETS_PASSPHRASE": "p"}
	pem := "-----BEGIN KEY-----\nabc\n-----END KEY-----"
	secretsCommand(t, dir, pem+"\r\n", vars, "set", "MY_KEY")
	f, err := store.Open(context.Background(), store.Config{Kind: store.File, File: filepath.Join(dir, "ringil", "secrets.enc"), PassphraseEnv: "P"}, secrets.Map{"P": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if v, _, err := f.Lookup(context.Background(), "MY_KEY"); err != nil || v != pem {
		t.Fatalf("%q %v", v, err)
	}
	h := storeHarness(t, dir, "", vars)
	if code := h.run("secrets", "set", "MY_KEY"); code == exitOK || !strings.Contains(h.err.String(), "no value") {
		t.Fatalf("an empty value: exit %d %s", code, h.err)
	}
}
