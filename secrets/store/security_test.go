// security_test.go: what a configuration or a store could do
//
// THREAT MODEL:
//   - The store's own contents never open it: the Vault token and the
//     file's passphrase come from the host's first resolver, so a value
//     planted in the store cannot stand in for them.
//   - A configuration (a file the user, or someone, edits) is checked
//     before anything opens: an unknown kind, a relative file, a bad
//     variable name are refused, not guessed.
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

// What opens the store comes from the base alone: a passphrase kept in
// the store itself is not one (it could not be read before it is open).
func TestThePassphraseComesFromTheBase(t *testing.T) {
	ctx := context.Background()
	c := Config{Kind: File, File: filepath.Join(t.TempDir(), "s.enc"), PassphraseEnv: "PASS"}
	f, err := Open(ctx, c, secrets.Map{})
	if err != nil {
		t.Fatal(err) // no file yet: nothing to open
	}
	closeLater(t, f)
	if err := f.Set(ctx, "KEY", "v"); !errors.HasCode(err, secrets.ErrMissing) {
		t.Fatalf("no passphrase: %v", err)
	}
}

func TestSecurityOpenChecksFirst(t *testing.T) {
	if s, err := Open(context.Background(), Config{Kind: File, File: "relative.enc", PassphraseEnv: "P"}, secrets.Map{}); s != nil || !errors.HasCode(err, secrets.ErrSource) {
		t.Fatalf("%v %v", s, err)
	}
}

func TestSecurityCacheNilBaseDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	var cache Cache
	t.Cleanup(func() { _ = cache.Close() })
	c := Config{Kind: File, File: filepath.Join(t.TempDir(), "s.enc"), PassphraseEnv: "PASS"}
	s, err := cache.Store(ctx, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "KEY", "v"); !errors.HasCode(err, secrets.ErrMissing) {
		t.Fatalf("nil base Set: %v", err)
	}
	_, _ = cache.Store(ctx, Config{}, secrets.Map{"PASS": "ok"})
	_, _ = cache.Store(ctx, Config{}, nil)
	if v, _, err := (latest{&cache}).Lookup(ctx, "PASS"); err != nil || v != "ok" {
		t.Fatalf("base clobbered by nil: %q %v", v, err)
	}
}
