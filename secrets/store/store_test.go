// store_test.go: the store a configuration names
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/encfile"
	"github.com/agilira/ringil-agent-sdk/secrets/keyring"
	"github.com/agilira/ringil-agent-sdk/secrets/vault"
)

func TestCheck(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "s.enc")
	for _, c := range []Config{
		{Kind: "safe"},
		{Kind: Keyring},
		{Kind: Vault, VaultURL: "https://v.example.com"},
		{Kind: Vault, VaultPath: "app"},
		{Kind: File, File: "rel/s.enc", PassphraseEnv: "P"},
		{Kind: File, File: abs},
		{Kind: File, File: abs, PassphraseEnv: "bad-name"},
		{Kind: Vault, VaultURL: "https://v.example.com", VaultPath: "app", TokenEnv: "1X"},
	} {
		if err := c.Check(); !errors.HasCode(err, secrets.ErrSource) && !errors.HasCode(err, secrets.ErrName) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for _, c := range []Config{{}, {Kind: Keyring, Service: "app"}, {Kind: File, File: abs, PassphraseEnv: "P"},
		{Kind: Vault, VaultURL: "https://v.example.com", VaultPath: "app"}} {
		if err := c.Check(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestOpenEachKind(t *testing.T) {
	ctx := context.Background()
	if s, err := Open(ctx, Config{}, secrets.Map{}); s != nil || err != nil {
		t.Fatalf("no store: %v %v", s, err)
	}
	k, _ := Open(ctx, Config{Kind: Keyring, Service: "app"}, secrets.Map{})
	v, _ := Open(ctx, Config{Kind: Vault, VaultURL: "https://v.example.com", VaultPath: "app"}, secrets.Map{})
	_, isKeyring := k.(*keyring.Keyring)
	_, isVault := v.(*vault.Vault)
	if !isKeyring || !isVault {
		t.Fatalf("%T %T", k, v)
	}
	f, err := Open(ctx, Config{Kind: File, File: filepath.Join(t.TempDir(), "s.enc"), PassphraseEnv: "PASS"}, secrets.Map{"PASS": "frase"})
	if _, ok := f.(*encfile.File); !ok || err != nil {
		t.Fatalf("file: %T %v", f, err)
	}
	if err := f.Set(ctx, "KEY", "v"); err != nil {
		t.Fatal(err)
	}
	checkOrder(t, f)
}

// checkOrder: the base wins over the store, which is read when the base
// has nothing.
func checkOrder(t *testing.T, f secrets.Store) {
	t.Helper()
	ctx := context.Background()
	if got, _, _ := Resolver(secrets.Map{"KEY": "from-base"}, f).Lookup(ctx, "KEY"); got != "from-base" {
		t.Fatalf("the base does not win: %q", got)
	}
	if got, _, _ := Resolver(secrets.Map{}, f).Lookup(ctx, "KEY"); got != "v" {
		t.Fatalf("the store is not read: %q", got)
	}
}

func TestCacheReopensOnAChange(t *testing.T) {
	ctx := context.Background()
	var cache Cache
	a, _ := cache.Store(ctx, Config{Kind: Keyring, Service: "app"}, secrets.Map{})
	b, _ := cache.Store(ctx, Config{Kind: Keyring, Service: "app"}, secrets.Map{})
	c, _ := cache.Store(ctx, Config{Kind: Keyring, Service: "other"}, secrets.Map{})
	if a == nil || a != b || c == a {
		t.Fatalf("a %p b %p c %p", a, b, c)
	}
	if s, err := cache.Store(ctx, Config{}, secrets.Map{}); s != nil || err != nil {
		t.Fatalf("no store: %v %v", s, err)
	}
}

// countingStore counts the lookups that reach it.
type countingStore struct {
	secrets.Map
	lookups int
}

func (c *countingStore) Lookup(ctx context.Context, name string) (string, bool, error) {
	c.lookups++
	return c.Map.Lookup(ctx, name)
}
func (c *countingStore) Set(_ context.Context, name, value string) error {
	c.Map[name] = value
	return nil
}
func (c *countingStore) Delete(_ context.Context, name string) error { delete(c.Map, name); return nil }

// Audit 22/11: a key read at every request of a model went to Vault each
// time. Answers are kept for the TTL; a Set through the store forgets.
func TestAnswersAreKept(t *testing.T) {
	ctx := context.Background()
	backend := &countingStore{Map: secrets.Map{"KEY": "v1"}}
	s := newTTLStore(backend, time.Hour)
	for range 5 {
		if v, _, _ := s.Lookup(ctx, "KEY"); v != "v1" {
			t.Fatalf("%q", v)
		}
		_, _, _ = s.Lookup(ctx, "ABSENT")
	}
	if backend.lookups != 2 {
		t.Fatalf("%d lookups reached the store", backend.lookups)
	}
	if err := s.Set(ctx, "KEY", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.Lookup(ctx, "KEY"); v != "v2" {
		t.Fatalf("after Set: %q", v)
	}
	if none := newTTLStore(backend, -1); none != secrets.Store(backend) {
		t.Fatal("a negative TTL still caches")
	}
}

// Audit 22/11: a store opened by the first run kept its base, so a token
// rotated in the host's env file was never seen. It reads the latest.
func TestTheLatestBaseOpens(t *testing.T) {
	ctx := context.Background()
	var cache Cache
	_, _ = cache.Store(ctx, Config{}, secrets.Map{"VAULT_TOKEN": "old"})
	_, _ = cache.Store(ctx, Config{}, secrets.Map{"VAULT_TOKEN": "new"})
	if v, _, _ := (latest{&cache}).Lookup(ctx, "VAULT_TOKEN"); v != "new" {
		t.Fatalf("%q", v)
	}
}

// Audit 22/11: every change of configuration opened a store and left the
// last open (a file descriptor each). One per configuration, reused, all
// closed by Close.
func TestOneStorePerConfiguration(t *testing.T) {
	ctx := context.Background()
	var cache Cache
	dir := t.TempDir()
	a := Config{Kind: File, File: filepath.Join(dir, "a.enc"), PassphraseEnv: "P"}
	b := Config{Kind: File, File: filepath.Join(dir, "b.enc"), PassphraseEnv: "P"}
	first, _ := cache.Store(ctx, a, secrets.Map{"P": "p"})
	for range 10 {
		_, _ = cache.Store(ctx, b, secrets.Map{"P": "p"})
		if again, _ := cache.Store(ctx, a, secrets.Map{"P": "p"}); again != first {
			t.Fatal("a store was opened again for the same configuration")
		}
	}
	if len(cache.stores) != 2 {
		t.Fatalf("%d stores", len(cache.stores))
	}
	if err := cache.Close(); err != nil || len(cache.stores) != 0 {
		t.Fatalf("close: %v, %d left", err, len(cache.stores))
	}
	if _, _, err := first.Lookup(ctx, "KEY"); err == nil {
		t.Fatal("a closed store answered")
	}
}
