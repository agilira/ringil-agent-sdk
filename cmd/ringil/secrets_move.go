// secrets_move.go: moving the keys to another store
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"cmp"
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/argus"
	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/provider/chatgpt"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/store"
)

// toEnv is the destination of a move to an env file: no store, the keys
// in a private file the host is given (a container's).
const toEnv = "env"

// secretNames are the secrets ringil may keep in a store: every
// provider's key (the built-ins' and the file's), the bot's and serve's
// tokens, the ChatGPT sign-in, and every ${NAME} of a place that holds
// credentials (headers, an OAuth secret). Never what opens a store (a
// passphrase, a Vault token), nor a reference too long to be a name.
func (f *config) secretNames() []string {
	names := map[string]bool{"RINGIL_TELEGRAM_TOKEN": true, serveTokenName: true, chatgptLogin: true, chatgptLogin + "_ACCESS": true}
	for name := range builtinProviders {
		names[builtinProviders[name].KeyEnv] = true
	}
	for name := range f.Providers {
		if ep, err := f.endpointNamed(name); err == nil {
			names[ep.keyName] = true
		}
	}
	addReferences(names, f.credentials())
	return f.withoutOpeners(names)
}

// mixedNames are the ${NAME}s of an MCP server's environment that are not
// among secretNames: a key, or a setting passed through (${HOME},
// ${PROJECT_DIR}); moved only from a store, which holds secrets alone
// (audit 23/11: from the environment a setting was copied, and then
// refused there forever).
func (f *config) mixedNames() []string {
	names := map[string]bool{}
	for _, m := range f.MCPServers {
		addReferences(names, []map[string]string{m.Env})
	}
	for _, name := range f.secretNames() {
		delete(names, name)
	}
	return f.withoutOpeners(names)
}

func addReferences(names map[string]bool, refs []map[string]string) {
	for _, values := range refs {
		for _, v := range values {
			for _, m := range reference.FindAllStringSubmatch(v, -1) {
				if secrets.ValidateName(m[1]) == nil { // one too long is no secret's: it would stop the move
					names[m[1]] = true
				}
			}
		}
	}
}

func (f *config) withoutOpeners(names map[string]bool) []string {
	for _, opener := range storeOpeners(f.Secrets) {
		delete(names, opener)
	}
	return slices.Sorted(maps.Keys(names))
}

// credentials are the values of the file that hold credentials.
func (f *config) credentials() []map[string]string {
	refs := []map[string]string{f.Memory.Headers}
	for _, m := range f.MCPServers {
		refs = append(refs, m.Headers)
		if m.OAuth != nil {
			refs = append(refs, map[string]string{"client_secret": m.OAuth.ClientSecret})
		}
	}
	for _, a := range f.Agents {
		refs = append(refs, a.Headers)
	}
	return refs
}

// storeOpeners are the names that open a store: never kept in one.
func storeOpeners(s secretsConfig) []string {
	return []string{"", "RINGIL_SECRETS_PASSPHRASE", "VAULT_TOKEN", s.PassphraseEnv, s.Vault.TokenEnv}
}

// move is one `ringil secrets move`: the old store, opened once before
// anything changes (the configuration reloads under it once the file is
// switched), and where the keys go.
type move struct {
	c    *cli
	ctx  *orpheus.Context
	base secrets.Resolver
	old  secretsConfig
	next secretsConfig
	from secrets.Store // the old store; nil: the environment and the --env-file
	file string        // a move to an env file: its path
}

// cmdSecretsMove copies the keys to another store (or a new env file),
// checks each there, switches ringil.yaml's secrets section, and only
// then offers to delete them from the old store. From no store, it copies
// what the environment and the --env-file hold (and deletes nothing
// there). The ChatGPT sign-in is held meanwhile: no refresh rotates it
// between the copy and the switch.
func (c *cli) cmdSecretsMove(ctx *orpheus.Context) error {
	m, err := c.newMove(ctx)
	if err != nil {
		return err
	}
	defer closeAll(m.from)
	dir, err := c.configDir()
	if err != nil {
		return err
	}
	done, err := m.copyAndSwitch(dir)
	if err != nil {
		return err
	}
	if err := c.line(m.report(done)); err != nil {
		return err
	}
	if err := m.warnShadows(done); err != nil {
		return err
	}
	if err := m.noteMixed(); err != nil {
		return err
	}
	return m.offerDelete(done)
}

// noteMixed names, after a move from the environment, the ${NAME}s of MCP
// servers' environments left there: settings or keys, ringil cannot tell.
func (m *move) noteMixed() error {
	mixed := m.c.conf().mixedNames()
	if m.from != nil || len(mixed) == 0 {
		return nil
	}
	return m.c.line("left in the environment, as they may be settings (MCP servers' env): " + strings.Join(mixed, ", ") + "; `ringil secrets set NAME` the ones that are keys")
}

// warnShadows names the keys moved that the environment or the
// --env-file still holds: with a store they stop ringil (the store's
// would be shadowed), and they are copies left in clear.
func (m *move) warnShadows(done secrets.Copied) error {
	if m.next.Store == "" {
		return nil
	}
	var left []string
	for _, name := range done.Names {
		if _, found, err := m.base.Lookup(m.ctx.Context(), name); err == nil && found {
			left = append(left, name)
		}
	}
	if len(left) == 0 {
		return nil
	}
	return m.c.line("still in your environment or env file, remove them (ringil refuses a key in both places): " + strings.Join(left, ", "))
}

// copyAndSwitch copies the keys and switches ringil.yaml holding the
// ChatGPT sign-in, so that no refresh rotates it in between; with the
// sign-in moved, it is held until a running serve has seen the switch,
// which it reads every two seconds (audit 23/11: a refresh in between went
// to the old store, and the new one kept a rotated token).
func (m *move) copyAndSwitch(dir string) (secrets.Copied, error) {
	unlock, err := chatgpt.FileLock(filepath.Join(dir, "ringil"), "chatgpt.lock")(m.ctx.Context())
	if err != nil {
		return secrets.Copied{}, errors.Wrap(err, ErrUsage, "the ChatGPT sign-in is busy: "+describe(err))
	}
	defer unlock()
	done, err := m.copy(dir)
	if err != nil {
		return done, errors.Wrap(err, ErrUsage, "nothing was switched: "+describe(err))
	}
	if err := m.c.switchStore(m.next); err != nil {
		if m.next.Store == "" {
			err = errors.Wrap(err, ErrUsage, describe(err)+"; delete "+m.file+", which holds the keys in clear")
		}
		return done, err
	}
	if slices.Contains(done.Names, chatgptLogin) {
		pause(m.ctx.Context(), 3*cmp.Or(m.c.configEvery, 2*time.Second))
	}
	return done, nil
}

// newMove reads the destination from the command line and opens the old
// store.
func (c *cli) newMove(ctx *orpheus.Context) (*move, error) {
	args := ctx.Positional()
	if len(args) != 1 {
		return nil, errors.New(ErrUsage, "name where: ringil secrets move keyring|file|vault|env")
	}
	m := &move{c: c, ctx: ctx, old: c.conf().Secrets, file: ctx.GetFlagString("to-file")}
	var err error
	m.next, err = movedTo(m.old, args[0], moveFlags{vaultURL: ctx.GetFlagString("vault-url"), vaultPath: ctx.GetFlagString("vault-path"), file: m.file})
	if err != nil {
		return nil, err
	}
	if m.base, err = c.baseResolver(ctx); err != nil {
		return nil, err
	}
	dir, err := c.configDir()
	if err != nil {
		return nil, err
	}
	m.from, err = store.Open(ctx.Context(), m.old.store(dir), m.base)
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "the store the keys are in cannot be opened: "+describe(err))
	}
	return m, nil
}

// source is where the keys are read: the old store itself (opened apart
// from the cache, so read from its backend), or the environment.
func (m *move) source() secrets.Resolver {
	if m.from == nil {
		return m.base
	}
	return m.from
}

// copy writes the keys where they go, and checks them there.
func (m *move) copy(dir string) (secrets.Copied, error) {
	names := m.c.conf().secretNames()
	if m.from != nil {
		names = append(names, m.c.conf().mixedNames()...)
	}
	if m.next.Store == "" {
		if err := m.noSignIn(); err != nil {
			return secrets.Copied{}, err
		}
		return m.toEnvFile(names)
	}
	to, err := store.Open(m.ctx.Context(), m.next.store(dir), m.base)
	if err != nil {
		return secrets.Copied{}, err
	}
	defer closeAll(to)
	return secrets.Copy(m.ctx.Context(), m.source(), to, names)
}

// noSignIn: a ChatGPT sign-in needs a store, where its refreshes are
// saved; an env file would hold a copy never read (audit 23/11).
func (m *move) noSignIn() error {
	_, found, err := m.source().Lookup(m.ctx.Context(), chatgptLogin)
	if err == nil && found {
		err = errors.New(ErrUsage, "the ChatGPT sign-in needs a store: `ringil logout chatgpt` first, or move to keyring, file or vault")
	}
	return err
}

// toEnvFile writes the keys found to a new private env file, read back.
func (m *move) toEnvFile(names []string) (secrets.Copied, error) {
	values := map[string]string{}
	var done secrets.Copied
	for _, name := range names {
		v, found, err := m.source().Lookup(m.ctx.Context(), name)
		if err != nil {
			return secrets.Copied{}, err
		}
		if found {
			values[name] = v
			done.Names = append(done.Names, name)
		}
	}
	return done, secrets.WriteEnvFile(m.file, values)
}

func (m *move) report(done secrets.Copied) string {
	n := strconv.Itoa(len(done.Names)) + " keys"
	if m.next.Store == "" {
		return "wrote " + n + " to " + m.file + " (private to you), read back; ringil.yaml now names no store: start ringil with --env-file " + m.file + ": " + strings.Join(done.Names, ", ")
	}
	return "copied " + n + " to the " + m.next.Store + " store, each read back; ringil.yaml now uses it: " + strings.Join(done.Names, ", ")
}

// offerDelete asks, defaulting to no, whether to delete the copied keys
// from the old store, the one opened before the switch; the environment
// and the env files are never touched.
func (m *move) offerDelete(done secrets.Copied) error {
	if m.from == nil || len(done.Names) == 0 {
		return nil
	}
	yes, err := m.c.prompter().Confirm("Delete them from the "+m.old.Store+" store? (they are in the new place, checked)", false)
	if err != nil || !yes {
		return err
	}
	for _, name := range done.Names {
		if err := m.from.Delete(m.ctx.Context(), name); err != nil {
			return errors.Wrap(err, ErrUsage, "deleting "+name+" from the old store: "+describe(err))
		}
	}
	return m.c.line("deleted from the " + m.old.Store + " store")
}

// moveFlags are the destination's settings that are not ringil.yaml's yet.
type moveFlags struct {
	vaultURL, vaultPath, file string
}

// movedTo is the secrets section after the move: kind as its store (none
// for an env file); Vault's address and path from the flags when given.
func movedTo(old secretsConfig, kind string, f moveFlags) (secretsConfig, error) {
	if kind == toEnv {
		return movedToEnv(old, f)
	}
	switch {
	case kind != store.Keyring && kind != store.File && kind != store.Vault:
		return secretsConfig{}, errors.New(ErrUsage, "the keys go to keyring, file, vault or env")
	case kind == old.Store:
		return secretsConfig{}, errors.New(ErrUsage, "the keys are already in the "+kind+" store")
	}
	next := old
	next.Store = kind
	next.Vault.URL = cmpOr(f.vaultURL, next.Vault.URL)
	next.Vault.Path = cmpOr(f.vaultPath, next.Vault.Path)
	return next, next.check()
}

// movedToEnv is the section with no store, the keys going to a new file.
func movedToEnv(old secretsConfig, f moveFlags) (secretsConfig, error) {
	switch {
	case f.file == "":
		return secretsConfig{}, errors.New(ErrUsage, "an env file needs its path: --to-file PATH (a new file)")
	case old.Store == "":
		return secretsConfig{}, errors.New(ErrUsage, "the keys are in no store: they are already where the environment reads them")
	}
	next := old
	next.Store = ""
	return next, nil
}

// switchStore writes the secrets section through argus's writer: the rest
// of the file, its comments and order stay; a backup of it beside it. No
// store removes the store key.
func (c *cli) switchStore(next secretsConfig) error {
	path := c.configName()
	w, err := argus.NewConfigWriter(path, argus.FormatYAML, nil)
	if err != nil {
		return errors.Wrap(err, ErrUsage, path+" cannot be edited ("+describe(err)+"): the keys are copied, the file still names the old store")
	}
	e := &edit{w: w}
	e.set("secrets.store", next.Store)
	if next.Store == store.Vault {
		e.merge("secrets.vault", "url", next.Vault.URL, "path", next.Vault.Path)
	}
	if e.err != nil {
		return errors.Wrap(e.err, ErrUsage, "cannot set the store in "+path+" ("+describe(e.err)+")")
	}
	if err := keepBackup(path); err != nil {
		return err
	}
	return w.WriteConfig()
}

// pause waits d, or until ctx ends.
func pause(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// closeAll closes a store that holds something open (an encrypted file:
// Windows keeps its directory while it is).
func closeAll(s secrets.Store) {
	if closer, ok := s.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}
