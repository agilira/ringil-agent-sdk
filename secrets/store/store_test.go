// store_test.go: the store a configuration names
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package store

import (
	"context"
	"path/filepath"
	"strings"
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
	closeLater(t, f)
	if err := f.Set(ctx, "KEY", "v"); err != nil {
		t.Fatal(err)
	}
	checkOrder(t, f)
}

// checkOrder: the store's key is the store's; the same name in the base
// too is refused (23/11: the base won in silence).
func checkOrder(t *testing.T, f secrets.Store) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := Resolver(secrets.Map{"KEY": "from-base"}, f).Lookup(ctx, "KEY"); !errors.HasCode(err, secrets.ErrShadow) {
		t.Fatalf("the base shadowed the store: %v", err)
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

// closeLater closes s at the end of the test: on Windows an open file
// keeps its directory from being removed.
func closeLater(t *testing.T, s secrets.Store) {
	t.Helper()
	if c, ok := s.(interface{ Close() error }); ok {
		t.Cleanup(func() { _ = c.Close() })
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

// Audit 22/11: a sign-in rotated by another process was read from the
// cache under the lock meant to see it. Fresh reads the store itself,
// and its answer refreshes the cache.
func TestFreshReadsTheStore(t *testing.T) {
	ctx := context.Background()
	backend := &countingStore{Map: secrets.Map{"LOGIN": "r1"}}
	cached := newTTLStore(backend, time.Hour)
	_, _, _ = cached.Lookup(ctx, "LOGIN")
	backend.Map["LOGIN"] = "r2" // another process rotated it
	if v, _, _ := cached.Lookup(ctx, "LOGIN"); v != "r1" {
		t.Fatalf("the cache: %q", v)
	}
	if v, _, _ := Fresh(cached).Lookup(ctx, "LOGIN"); v != "r2" {
		t.Fatalf("fresh: %q", v)
	}
	if v, _, _ := cached.Lookup(ctx, "LOGIN"); v != "r2" {
		t.Fatalf("the cache after a fresh read: %q", v)
	}
	if err := Fresh(cached).Set(ctx, "LOGIN", "r3"); err != nil || backend.Map["LOGIN"] != "r3" {
		t.Fatalf("set: %v", err)
	}
	if Fresh(backend) != secrets.Store(backend) {
		t.Fatal("a store not from a cache was wrapped")
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

// 23/11: the store's keys are the store's. The same name in the
// environment too is an error naming it, never a silent choice; a name the
// store lacks comes from the environment; a store that fails stops the
// lookup.
func TestTheStoreIsNotShadowed(t *testing.T) {
	ctx := context.Background()
	keys := &countingStore{Map: secrets.Map{"IN_STORE": "store-value", "BOTH": "store-both"}}
	res := Resolver(secrets.Map{"BOTH": "env-both", "ONLY_ENV": "env-value"}, keys)
	for name, want := range map[string]string{"IN_STORE": "store-value", "ONLY_ENV": "env-value", "NOWHERE": ""} {
		if v, ok, err := res.Lookup(ctx, name); err != nil || ok != (want != "") || v != want {
			t.Errorf("%s: %q %v %v", name, v, ok, err)
		}
	}
	checkShadowed(t, res, "BOTH", "-both")
	if _, _, err := Resolver(secrets.Map{"K": "env"}, brokenStore{}).Lookup(ctx, "K"); err == nil {
		t.Fatal("a failing store fell through to the environment")
	}
}

// checkShadowed: name is refused, naming it and no value (value is part of
// the base's).
func checkShadowed(t *testing.T, res secrets.Resolver, name, value string) {
	t.Helper()
	v, _, err := res.Lookup(context.Background(), name)
	if !errors.HasCode(err, secrets.ErrShadow) || v != "" || strings.Contains(err.Error(), value) || !strings.Contains(err.Error(), name) {
		t.Fatalf("%q %v", v, err)
	}
}

type brokenStore struct{}

func (brokenStore) Lookup(context.Context, string) (string, bool, error) {
	return "", false, errors.New(secrets.ErrLookup, "down")
}
func (brokenStore) Set(context.Context, string, string) error { return nil }
func (brokenStore) Delete(context.Context, string) error      { return nil }

// 23/11: a provider's key comes from the store alone once there is one: a
// key planted in the environment for a provider the store lacks is
// refused, naming it.
func TestStoreOnly(t *testing.T) {
	ctx := context.Background()
	keys := &countingStore{Map: secrets.Map{"IN_STORE": "s"}}
	res := StoreOnly(secrets.Map{"PLANTED": "attacker", "BOTH": "b"}, keys)
	keys.Map["BOTH"] = "s"
	if v, ok, err := res.Lookup(ctx, "IN_STORE"); err != nil || !ok || v != "s" {
		t.Fatalf("%q %v %v", v, ok, err)
	}
	checkShadowed(t, res, "PLANTED", "attacker")
	if _, _, _, err := res.(interface {
		LookupSource(context.Context, string) (string, bool, bool, error)
	}).LookupSource(ctx, "PLANTED"); !errors.HasCode(err, secrets.ErrShadow) {
		t.Fatalf("LookupSource let the planted key through: %v", err)
	}
	checkShadowed(t, res, "BOTH", "\x00")
	if _, ok, err := res.Lookup(ctx, "NOWHERE"); err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
	if v, _, err := StoreOnly(secrets.Map{"K": "env"}, nil).Lookup(ctx, "K"); err != nil || v != "env" {
		t.Fatalf("no store: %q %v", v, err)
	}
}

// The resolver tells where a value came from, from the read that gave it.
func TestLookupSource(t *testing.T) {
	keys := &countingStore{Map: secrets.Map{"IN_STORE": "s"}}
	base := &countingStore{Map: secrets.Map{"E": "e"}}
	assertSource(t, Resolver(base, keys), "E", "e", false)
	if base.lookups != 1 {
		t.Fatalf("base lookups: %d", base.lookups)
	}
	assertSource(t, Resolver(secrets.Map{}, keys), "IN_STORE", "s", true)
}

func assertSource(t *testing.T, res secrets.Resolver, name, want string, wantStore bool) {
	t.Helper()
	v, found, fromStore, err := res.(interface {
		LookupSource(context.Context, string) (string, bool, bool, error)
	}).LookupSource(context.Background(), name)
	if err != nil || !found || fromStore != wantStore || v != want {
		t.Fatalf("%s: %q %v %v %v", name, v, found, fromStore, err)
	}
}
