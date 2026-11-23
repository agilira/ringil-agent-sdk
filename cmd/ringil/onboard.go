// onboard.go: the guided first setup
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/agilira/argus"
	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/provider/chatgpt"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/store"
)

// setup is one guided setup: the answers so far, and the store the keys
// go into as soon as they are given (never to the disk in clear).
type setup struct {
	c   *cli
	ctx context.Context
	p   orpheus.Prompter

	path       string // ringil.yaml
	secrets    secretsConfig
	base       secrets.Map // what opens the store (a passphrase, a token), for this setup only
	keys       secrets.Store
	provider   providerChoice
	endpoint   endpoint // the chosen provider, resolved
	custom     bool     // a server of one's own: written to the providers section
	allowHTTP  bool     // plain http to another host, which the user allowed
	key        string   // the model's key, to list the models and try one
	model      string
	prompt     string
	memory     memoryStore
	embedModel string         // at the chat model's server (embed_model)
	embedder   embedderConfig // at Ollama
	owners     []string
	composio   bool             // Composio's key was kept
	dropOAuth  string           // server whose OAuth token is dropped once ringil.yaml writes
	session    *chatgpt.Session // the ChatGPT sign-in, for that provider
}

// cmdSetup asks the questions a first start needs, in order: where the
// keys are kept, the model and its key, the system prompt, the memory,
// a Telegram bot; then writes ringil.yaml (keeping what else it holds)
// and tries the model.
func (c *cli) cmdSetup(ctx *orpheus.Context) error {
	path, _ := c.configPath(ctx)
	if path == "" {
		return errors.New(ErrUsage, "no config directory to write ringil.yaml in")
	}
	if err := argus.ValidateSecurePath(path); err != nil {
		return errors.New(ErrUsage, "not a path ringil writes its configuration to: "+describe(err))
	}
	s := &setup{c: c, ctx: ctx.Context(), p: c.prompter(), path: path, base: secrets.Map{}}
	steps := []func() error{s.intro, s.chooseStore, s.chooseProvider, s.askCredential, s.chooseModel,
		s.askPrompt, s.chooseEmbedder, s.chooseMemory, s.askTelegram,
		s.askComposio, s.write, s.tryModel}
	defer s.closeStore()
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return s.done()
}

// done tells what is left to the user: the variable that opens the store
// (the passphrase, the Vault token), which ringil never writes down. This
// process keeps it, so a chat started now goes on (audit 22/11: it
// stopped at once, the passphrase being the setup's alone).
func (s *setup) done() error {
	getenv := s.c.getenv
	s.c.getenv = func(k string) string {
		if v, ok := s.base[k]; ok && getenv(k) == "" {
			return v
		}
		return getenv(k)
	}
	msg := "ringil is set up: run `ringil` to start."
	for _, name := range []string{"RINGIL_SECRETS_PASSPHRASE", "VAULT_TOKEN"} {
		if _, ok := s.base[name]; ok {
			msg = "ringil is set up. Before starting it, set " + name + " in your environment (or an --env-file of mode 600): ringil never writes it down."
		}
	}
	return s.c.line(msg)
}

// firstRun offers the setup to a person who starts ringil with nothing
// set up (no ringil.yaml, no RINGIL_MODEL, no --env-file), then goes on.
func (c *cli) firstRun(next func(*orpheus.Context) error) func(*orpheus.Context) error {
	return func(ctx *orpheus.Context) error {
		if !c.notSetUp(ctx) {
			return next(ctx)
		}
		yes, err := c.prompter().Confirm("ringil is not set up yet: set it up now?", true)
		if err != nil || !yes {
			return err
		}
		if err := c.cmdSetup(ctx); err != nil {
			return err
		}
		return next(ctx)
	}
}

// notSetUp: a person at the terminal, and nothing that sets ringil up.
func (c *cli) notSetUp(ctx *orpheus.Context) bool {
	path, explicit := c.configPath(ctx)
	if explicit || path == "" || c.getenv("RINGIL_MODEL") != "" || ctx.GetGlobalFlagString("env-file") != "" || !c.interactive() {
		return false
	}
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

// prompter asks on the CLI's input and output; secrets without echo at a
// terminal. One for the process: it reads ahead, so a second would find
// the answers gone.
func (c *cli) prompter() orpheus.Prompter {
	if c.asker == nil {
		fd := -1
		if f, ok := c.in.(*os.File); ok {
			fd = int(f.Fd()) // #nosec G115 -- a file descriptor
		}
		c.asker = orpheus.NewPrompterFrom(c.in, c.errOut, fd)
	}
	return c.asker
}

func (s *setup) intro() error {
	if err := s.c.line("Setting up ringil. Every answer can be changed later with `ringil setup`."); err != nil {
		return err
	}
	if _, err := os.Stat(s.path); err != nil {
		return nil
	}
	ok, err := s.p.Confirm(s.path+" exists: update it? (what this setup does not ask stays as it is)", true)
	if err == nil && !ok {
		err = errors.New(ErrUsage, "setup stopped: "+s.path+" left as it is")
	}
	return err
}

// choose asks until the answer is one of options (three tries).
func (s *setup) choose(question string, options []string) (int, error) {
	var err error
	for range 3 {
		var i int
		if i, err = s.p.Choose(question, options); err == nil {
			return i, nil
		}
		if s.ctx.Err() != nil {
			return 0, s.ctx.Err()
		}
		if lerr := s.c.line("  " + err.Error()); lerr != nil {
			return 0, lerr
		}
	}
	return 0, errors.Wrap(err, ErrUsage, "no valid answer")
}

// ask asks until the answer passes check (three tries).
func (s *setup) ask(question, def string, check func(string) error) (string, error) {
	var err error
	for range 3 {
		var v string
		if v, err = s.p.Ask(question, def); err != nil {
			return "", err
		}
		v = strings.TrimSpace(v)
		if err = check(v); err == nil {
			return v, nil
		}
		if lerr := s.c.line("  " + err.Error()); lerr != nil {
			return "", lerr
		}
	}
	return "", errors.Wrap(err, ErrUsage, "no valid answer")
}

func (s *setup) chooseStore() error {
	options := []string{"the system's keyring (Keychain, Credential Manager, GNOME Keyring/KWallet)",
		"an encrypted file, with a passphrase (a server without a keyring)", "HashiCorp Vault"}
	i, err := s.choose("Where should ringil keep your keys?", options)
	if err != nil {
		return err
	}
	switch i {
	case 0:
		s.secrets = secretsConfig{Store: store.Keyring}
	case 1:
		err = s.fileStore()
	case 2:
		err = s.vaultStore()
	}
	if err != nil {
		return err
	}
	return s.openStore()
}

func (s *setup) fileStore() error {
	pass, err := s.p.AskSecret("A passphrase for the file (at least 12 characters)")
	for err == nil && len(pass) < 12 {
		if err = s.c.line("  at least 12 characters"); err == nil {
			pass, err = s.p.AskSecret("A passphrase for the file")
		}
	}
	if err != nil {
		return err
	}
	again, err := s.p.AskSecret("The passphrase again")
	if err != nil {
		return err
	}
	if again != pass {
		return errors.New(ErrUsage, "the two passphrases differ")
	}
	s.secrets = secretsConfig{Store: store.File}
	s.base["RINGIL_SECRETS_PASSPHRASE"] = pass
	return s.c.line("  ringil reads the passphrase from RINGIL_SECRETS_PASSPHRASE: set it in your environment (or an --env-file) before starting it.")
}

func (s *setup) vaultStore() error {
	url, err := s.ask("Vault's address", "https://", func(v string) error {
		_, err := store.Open(s.ctx, store.Config{Kind: store.Vault, VaultURL: v, VaultPath: "x"}, secrets.Map{})
		return err
	})
	if err != nil {
		return err
	}
	path, err := s.ask("The path of ringil's secrets in it", "ringil", func(v string) error {
		return store.Config{Kind: store.Vault, VaultURL: url, VaultPath: v}.Check()
	})
	if err != nil {
		return err
	}
	s.secrets = secretsConfig{Store: store.Vault, Vault: vaultConfig{URL: url, Path: path}}
	if s.c.getenv("VAULT_TOKEN") == "" {
		t, err := s.p.AskSecret("A Vault token for this setup (ringil reads VAULT_TOKEN from your environment afterwards)")
		if err != nil {
			return err
		}
		s.base["VAULT_TOKEN"] = t
	}
	return nil
}

// openStore opens the chosen store for this setup and checks it answers.
func (s *setup) openStore() error {
	dir, err := s.c.configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "ringil"), 0o700); err != nil {
		return errors.Wrap(err, ErrUsage, "cannot make the config directory")
	}
	base := secrets.Chain(lookupEnv{s.c.getenv}, s.base)
	keys, err := store.Open(s.ctx, s.secrets.store(dir), base)
	if err != nil {
		return err
	}
	s.keys = keys
	if _, _, err := secrets.Lookup(s.ctx, keys, "RINGIL_SETUP_PROBE"); err != nil {
		s.closeStore()
		return errors.Wrap(err, ErrUsage, "the store does not answer: "+err.Error())
	}
	return nil
}

// closeStore releases the store this setup opened: an encrypted file holds
// its directory open, which Windows will not let anyone remove meanwhile.
func (s *setup) closeStore() {
	if c, ok := s.keys.(interface{ Close() error }); ok {
		_ = c.Close() // #nosec G104 -- nothing is written on close
	}
	s.keys = nil
}
