// secrets.go: where the keys are kept, for every part of ringil
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"
	"golang.org/x/term"

	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/encfile"
	"github.com/agilira/ringil-agent-sdk/secrets/keyring"
	"github.com/agilira/ringil-agent-sdk/secrets/vault"
)

// secretsConfig is ringil.yaml's secrets section: the store the keys are
// kept in, read after the environment and the --env-file (which win, so a
// key can be overridden for one run). Every part of ringil reads its keys
// through it: the CLI, serve and its channels (a Telegram bot's token
// too), the remote agents and MCP servers.
type secretsConfig struct {
	// Store is keyring (the system's), vault, or file (encrypted); empty,
	// the environment and the --env-file alone.
	Store string `argus:"store"`
	// Service names ringil's entries in the keyring; default "ringil".
	Service string      `argus:"keyring_service"`
	Vault   vaultConfig `argus:"vault"`
	// File is the encrypted file (default <config>/ringil/secrets.enc),
	// opened with the passphrase in PassphraseEnv (default
	// RINGIL_SECRETS_PASSPHRASE, from the environment or the --env-file).
	File          string `argus:"file"`
	PassphraseEnv string `argus:"passphrase_env"`
}

type vaultConfig struct {
	URL       string `argus:"url"`
	Mount     string `argus:"mount"`
	Path      string `argus:"path"`
	TokenEnv  string `argus:"token_env"` // default VAULT_TOKEN, from the environment or the --env-file
	Namespace string `argus:"namespace"`
	AllowHTTP bool   `argus:"allow_http"`
}

func (s secretsConfig) check() error {
	switch s.Store {
	case "", "keyring", "file":
	case "vault":
		if s.Vault.URL == "" || s.Vault.Path == "" {
			return errors.New(ErrUsage, "secrets: the vault store needs vault.url and vault.path")
		}
	default:
		return errors.New(ErrUsage, "secrets.store is keyring, vault or file")
	}
	for _, name := range []string{s.PassphraseEnv, s.Vault.TokenEnv} {
		if err := validKeyName(name); err != nil {
			return errors.Wrap(err, ErrUsage, "secrets: "+err.Error())
		}
	}
	if s.File != "" && !filepath.IsAbs(s.File) {
		return errors.New(ErrUsage, "secrets.file is an absolute path")
	}
	return nil
}

// secretStore is the store opened for one secrets section, shared by the
// runs of serve: an encrypted file costs its key derivation once.
type secretStore struct {
	mu    sync.Mutex
	conf  secretsConfig
	store secrets.Store
}

// keyStore is the configured store, opened once per configuration; nil when
// none is configured. base resolves what opens it (the Vault token, the
// file's passphrase), never the store itself.
func (c *cli) keyStore(ctx context.Context, base secrets.Resolver) (secrets.Store, error) {
	conf := c.conf().Secrets
	if conf.Store == "" {
		return nil, nil
	}
	s := c.secrets
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil && s.conf == conf {
		return s.store, nil
	}
	store, err := c.openStore(ctx, conf, base)
	if err != nil {
		return nil, err
	}
	s.store, s.conf = store, conf
	return store, nil
}

func (c *cli) openStore(ctx context.Context, conf secretsConfig, base secrets.Resolver) (secrets.Store, error) {
	switch conf.Store {
	case "keyring":
		return keyring.New(cmpOr(conf.Service, "ringil"))
	case "vault":
		return vault.New(vault.Config{URL: conf.Vault.URL, Mount: conf.Vault.Mount, Path: conf.Vault.Path, Namespace: conf.Vault.Namespace,
			AllowHTTP: conf.Vault.AllowHTTP, Token: secrets.KeyFunc(base, cmpOr(conf.Vault.TokenEnv, "VAULT_TOKEN"))})
	}
	path := conf.File
	if path == "" {
		dir, err := c.configDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(dir, "ringil", "secrets.enc")
	}
	name := cmpOr(conf.PassphraseEnv, "RINGIL_SECRETS_PASSPHRASE")
	return encfile.New(ctx, encfile.Config{Path: path, Passphrase: func(ctx context.Context) (string, error) {
		v, found, err := secrets.Lookup(ctx, base, name)
		if err == nil && (!found || v == "") {
			err = errors.New(secrets.ErrMissing, "the secrets file needs its passphrase in "+name)
		}
		return v, err
	}})
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// cmdSecretsSet saves a secret in the configured store, its value read
// from standard input: never from an argument, which would stay in the
// shell's history and the process list; typed without echo.
func (c *cli) cmdSecretsSet(ctx *orpheus.Context) error {
	store, name, err := c.namedStore(ctx)
	if err != nil {
		return err
	}
	value, err := c.readSecret(name)
	if err != nil {
		return err
	}
	if err := store.Set(ctx.Context(), name, value); err != nil {
		return err
	}
	return c.line("saved " + name + " in the " + c.conf().Secrets.Store + " store")
}

// cmdSecretsRemove deletes a secret from the configured store.
func (c *cli) cmdSecretsRemove(ctx *orpheus.Context) error {
	store, name, err := c.namedStore(ctx)
	if err != nil {
		return err
	}
	if err := store.Delete(ctx.Context(), name); err != nil {
		return err
	}
	return c.line("removed " + name + " from the " + c.conf().Secrets.Store + " store")
}

func (c *cli) namedStore(ctx *orpheus.Context) (secrets.Store, string, error) {
	args := ctx.Positional()
	if len(args) != 1 {
		return nil, "", errors.New(ErrUsage, "name one secret, such as OPENAI_API_KEY")
	}
	if err := secrets.ValidateName(args[0]); err != nil {
		return nil, "", errors.Wrap(err, ErrUsage, err.Error())
	}
	base, err := c.baseResolver(ctx)
	if err != nil {
		return nil, "", err
	}
	store, err := c.keyStore(ctx.Context(), base)
	if err != nil {
		return nil, "", err
	}
	if store == nil {
		return nil, "", errors.New(ErrUsage, "no store to keep secrets in: set secrets.store in ringil.yaml (keyring, vault or file)")
	}
	return store, args[0], nil
}

// readSecret is one line of standard input: without echo at a terminal.
func (c *cli) readSecret(name string) (string, error) {
	if f, ok := c.in.(*os.File); ok && term.IsTerminal(int(f.Fd())) { // #nosec G115 -- a file descriptor
		if err := c.line("value of " + name + " (not shown):"); err != nil {
			return "", err
		}
		v, err := term.ReadPassword(int(f.Fd())) // #nosec G115 -- a file descriptor
		return strings.TrimSpace(string(v)), err
	}
	line, err := bufio.NewReader(io.LimitReader(c.in, 64<<10+2)).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	v := strings.TrimRight(line, "\r\n")
	if v == "" {
		return "", errors.New(ErrUsage, "no value on standard input")
	}
	return v, nil
}
