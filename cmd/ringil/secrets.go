// secrets.go: where the keys are kept, for every part of ringil
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/agilira/argus"
	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"
	"golang.org/x/term"

	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/store"
)

// secretsConfig is ringil.yaml's secrets section: the store the keys are
// kept in (the SDK's secrets/store, as the TUI and the app read it), after
// the environment and the --env-file (which win, so a key can be
// overridden for one run). Every part of ringil reads its keys through
// it: the CLI, serve and its channels (a Telegram bot's token too), the
// remote agents and MCP servers.
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
	if s.Store == store.File && s.File != "" && !filepath.IsAbs(s.File) {
		return errors.New(ErrUsage, "secrets.file is an absolute path")
	}
	if s.Store == store.File && s.File != "" {
		if err := argus.ValidateSecurePath(s.File); err != nil {
			return errors.New(ErrUsage, "secrets.file is not a path ringil keeps keys in: "+s.File)
		}
	}
	here, _ := filepath.Abs(".") // stands for the config directory: absolute on every system
	if err := s.store(here).Check(); err != nil {
		return errors.Wrap(err, ErrUsage, "secrets: "+err.Error())
	}
	return nil
}

// store is the section as the SDK's store.Config, with ringil's defaults
// for its kind: the keyring service "ringil"; the file in the config
// directory, its passphrase in RINGIL_SECRETS_PASSPHRASE.
func (s secretsConfig) store(configDir string) store.Config {
	c := store.Config{Kind: s.Store}
	switch s.Store {
	case store.Keyring:
		c.Service = cmpOr(s.Service, "ringil")
	case store.Vault:
		c.VaultURL, c.VaultMount, c.VaultPath, c.TokenEnv = s.Vault.URL, s.Vault.Mount, s.Vault.Path, s.Vault.TokenEnv
		c.Namespace, c.AllowHTTP = s.Vault.Namespace, s.Vault.AllowHTTP
	case store.File:
		c.File = cmpOr(s.File, filepath.Join(configDir, "ringil", "secrets.enc"))
		c.PassphraseEnv = cmpOr(s.PassphraseEnv, "RINGIL_SECRETS_PASSPHRASE")
	}
	return c
}

// keyStore is the configured store, opened once per configuration (and
// shared with serve's runs); nil when none is configured. base resolves
// what opens it, never the store itself.
func (c *cli) keyStore(ctx context.Context, base secrets.Resolver) (secrets.Store, error) {
	conf := c.conf().Secrets
	if conf.Store == "" {
		return nil, nil
	}
	dir, err := c.configDir()
	if err != nil {
		return nil, err
	}
	return c.secrets.Store(ctx, conf.store(dir), base)
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

// readSecret is the value on standard input, up to 64 KiB: at a
// terminal one line typed without echo; from a pipe all of it (a PEM key
// spans lines), less one final line break. Nothing else is trimmed, the
// same either way; an empty value is refused.
func (c *cli) readSecret(name string) (string, error) {
	var v string
	if f, ok := c.in.(*os.File); ok && term.IsTerminal(int(f.Fd())) { // #nosec G115 -- a file descriptor
		if err := c.line("value of " + name + " (not shown):"); err != nil {
			return "", err
		}
		typed, err := term.ReadPassword(int(f.Fd())) // #nosec G115 -- a file descriptor
		if err != nil {
			return "", err
		}
		v = string(typed)
	} else {
		data, err := io.ReadAll(io.LimitReader(c.in, 64<<10+3))
		if err != nil {
			return "", err
		}
		v = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	}
	if v == "" {
		return "", errors.New(ErrUsage, "no value given for "+name)
	}
	return v, nil
}
