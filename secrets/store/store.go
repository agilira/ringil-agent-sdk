// store.go: the secrets store an application is configured with
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package store opens the [secrets.Store] an application's configuration
// names (the OS keyring, Vault, an encrypted file), so that every
// program built on the SDK (a terminal UI, a desktop app, a server, a
// CLI) reads its keys from the same place, the same way: [Config] is
// that choice, [Open] the store it names, [Cache] one store per
// configuration, and [Resolver] the lookup order: what the host resolves
// first (its environment, an env file), then the store.
//
// What opens the store (the Vault token, the file's passphrase) is read
// from that first resolver, never from the store itself.
package store

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/encfile"
	"github.com/agilira/ringil-agent-sdk/secrets/keyring"
	"github.com/agilira/ringil-agent-sdk/secrets/vault"
)

// Kinds of store.
const (
	Keyring = "keyring"
	Vault   = "vault"
	File    = "file"
)

// Config is the store: Kind, and the settings of that kind. The empty
// Config is no store.
type Config struct {
	// Kind is Keyring, Vault or File; empty, no store.
	Kind string
	// Service names the application's entries in the keyring.
	Service string
	// Vault: its address, KV version 2 mount (default "secret") and path,
	// the variable holding its token (default VAULT_TOKEN), the namespace
	// (Vault Enterprise, HCP), and plain http to a remote host.
	VaultURL, VaultMount, VaultPath, TokenEnv, Namespace string
	AllowHTTP                                            bool
	// File is the encrypted file (an absolute path), opened with the
	// passphrase in the variable PassphraseEnv.
	File, PassphraseEnv string
}

// Check reports what is missing or wrong in c, before anything opens.
func (c Config) Check() error {
	if err := c.checkKind(); err != nil {
		return err
	}
	for _, name := range []string{c.TokenEnv, c.PassphraseEnv} {
		if name == "" {
			continue
		}
		if err := secrets.ValidateName(name); err != nil {
			return err
		}
	}
	return nil
}

// checkKind: the kind is known and has what it needs.
func (c Config) checkKind() error {
	switch c.Kind {
	case "":
		return nil
	case Keyring:
		_, err := keyring.New(c.Service) // its name rules
		return err
	case Vault:
		return need(c.VaultURL != "" && c.VaultPath != "", "store: Vault needs its URL and the path of the secrets")
	case File:
		return need(filepath.IsAbs(c.File) && c.PassphraseEnv != "", "store: the encrypted file needs an absolute path and the variable of its passphrase")
	}
	return errors.New(secrets.ErrSource, "store: the kind is keyring, vault or file")
}

func need(ok bool, what string) error {
	if ok {
		return nil
	}
	return errors.New(secrets.ErrSource, what)
}

// Open is the store c names, nil for none; base resolves what opens it.
func Open(ctx context.Context, c Config, base secrets.Resolver) (secrets.Store, error) {
	if err := c.Check(); err != nil {
		return nil, err
	}
	switch c.Kind {
	case "":
		return nil, nil
	case Keyring:
		return keyring.New(c.Service)
	case Vault:
		tokenEnv := c.TokenEnv
		if tokenEnv == "" {
			tokenEnv = "VAULT_TOKEN"
		}
		return vault.New(vault.Config{URL: c.VaultURL, Mount: c.VaultMount, Path: c.VaultPath, Namespace: c.Namespace,
			AllowHTTP: c.AllowHTTP, Token: secrets.KeyFunc(base, tokenEnv)})
	}
	return encfile.New(ctx, encfile.Config{Path: c.File, Passphrase: func(ctx context.Context) (string, error) {
		v, found, err := secrets.Lookup(ctx, base, c.PassphraseEnv)
		if err == nil && (!found || v == "") {
			err = errors.New(secrets.ErrMissing, "the encrypted file needs its passphrase in "+c.PassphraseEnv)
		}
		return v, err
	}})
}

// Resolver is base, then store: what the host resolves wins (an
// environment variable overrides the store for one run). Give it to
// what reads keys; settings that are not secrets are better read from
// base alone, so that a store that cannot answer stops only what needs a
// key.
func Resolver(base secrets.Resolver, store secrets.Store) secrets.Resolver {
	if store == nil {
		return base
	}
	return secrets.Chain(base, store)
}

// DefaultTTL is how long the Cache's stores keep what they read: a key
// read at every request of a model is fetched from Vault or the keyring
// once a minute, and a rotated one is picked up within that.
const DefaultTTL = time.Minute

// Cache keeps one store per configuration asked: a program whose
// configuration can change while it runs opens a store once per
// configuration (going back to one reuses it), and the requests of a
// server share it (an encrypted file costs its key derivation once).
// What it opens reads its token or passphrase through the base of the
// latest call, so a token rotated in the host's environment or env file
// is picked up. Close closes them. Its zero value is ready.
type Cache struct {
	// TTL is how long a store keeps what it read; 0 is DefaultTTL,
	// negative keeps nothing.
	TTL time.Duration

	mu     sync.Mutex
	stores map[Config]secrets.Store
	base   atomic.Pointer[secrets.Resolver]
}

// Store is the store of c, opened on its first use; base is the host's
// first resolver, as now.
func (k *Cache) Store(ctx context.Context, c Config, base secrets.Resolver) (secrets.Store, error) {
	k.base.Store(&base)
	if c.Kind == "" {
		return nil, nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if s, ok := k.stores[c]; ok {
		return s, nil
	}
	s, err := Open(ctx, c, latest{k})
	if err != nil {
		return nil, err
	}
	if k.stores == nil {
		k.stores = map[Config]secrets.Store{}
	}
	cached := newTTLStore(s, k.ttl())
	k.stores[c] = cached
	return cached, nil
}

func (k *Cache) ttl() time.Duration {
	if k.TTL == 0 {
		return DefaultTTL
	}
	return k.TTL
}

// Close closes every store it opened that holds resources (an encrypted
// file's directory): call it when the program ends.
func (k *Cache) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	var first error
	for c, s := range k.stores {
		if cl, ok := s.(interface{ Close() error }); ok {
			if err := cl.Close(); err != nil && first == nil {
				first = err
			}
		}
		delete(k.stores, c)
	}
	return first
}

// latest is the base of the Cache's latest call.
type latest struct{ k *Cache }

func (l latest) Lookup(ctx context.Context, name string) (string, bool, error) {
	base := l.k.base.Load()
	if base == nil {
		return "", false, nil
	}
	return (*base).Lookup(ctx, name)
}
