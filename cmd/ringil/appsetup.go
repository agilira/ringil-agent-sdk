// appsetup.go: the app's first start, plug and play
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"unicode/utf8"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/store"
)

// mountSetup mounts, behind the guard:
//
//	GET /v1/setup                    {"ready","models","key_set","instructions","secrets_store"}: what the first start still needs
//	GET /v1/settings/instructions    {"text","path"}: the user's own instructions, added to ringil's prompt
//	PUT /v1/settings/instructions    {"text"}: written to their file (made in the config directory the first time)
func (c *cli) mountSetup(ctx *orpheus.Context, srv interface {
	Mount(string, http.Handler)
}) {
	srv.Mount("GET /v1/setup", c.setupStatus(ctx))
	srv.Mount("GET /v1/settings/instructions", http.HandlerFunc(c.getInstructions))
	srv.Mount("PUT /v1/settings/instructions", http.HandlerFunc(c.putInstructions))
}

// Setup is what GET /v1/setup answers: Ready once there is a model with
// its key; the app shows its first screen until then.
type Setup struct {
	Ready        bool   `json:"ready"`
	Models       bool   `json:"models"`
	KeySet       bool   `json:"key_set"`
	Instructions bool   `json:"instructions"`
	SecretsStore string `json:"secrets_store"`
}

func (c *cli) setupStatus(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := c.conf()
		out := Setup{Models: len(f.Models) > 0, SecretsStore: f.Secrets.Store}
		if m, ok := first(f.Models); ok {
			if ep, err := f.endpointNamed(m.provider); err == nil {
				res, _ := c.resolver(octx)
				out.KeySet = res != nil && c.reachableIn(r.Context(), res, ep)
			}
		}
		if text, _ := c.ownInstructions(); text != "" {
			out.Instructions = true
		}
		out.Ready = out.Models && out.KeySet
		writeJSON(w, http.StatusOK, out)
	}
}

// instructionsPath is the user's instructions file: ringil.yaml's, else
// <config>/ringil/instructions.md.
func (c *cli) instructionsPath() (path string, named bool, err error) {
	if p := c.conf().Instructions; p != "" {
		return p, true, nil
	}
	root, err := c.stateRoot()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(root, "instructions.md"), false, nil
}

// ownInstructions is the text of the user's instructions file.
func (c *cli) ownInstructions() (string, error) {
	path, _, err := c.instructionsPath()
	if err != nil {
		return "", err
	}
	data, err := readPrivate(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(data), err
}

type instructionsOut struct {
	Text string `json:"text"`
	Path string `json:"path"`
}

func (c *cli) getInstructions(w http.ResponseWriter, _ *http.Request) {
	path, _, err := c.instructionsPath()
	if err != nil {
		fail(w, http.StatusConflict, "SETTINGS_NO_FILE", describe(err))
		return
	}
	text, err := c.ownInstructions()
	if err != nil {
		fail(w, http.StatusInternalServerError, "SETTINGS_INSTRUCTIONS", describe(err))
		return
	}
	writeJSON(w, http.StatusOK, instructionsOut{Text: text, Path: path})
}

type instructionsIn struct {
	Text string `json:"text"`
}

// putInstructions writes the user's instructions; the first time, the
// file is made and ringil.yaml names it, checked as every change is. The
// watcher applies them to the runs that start next.
func (c *cli) putInstructions(w http.ResponseWriter, r *http.Request) {
	var in instructionsIn
	if !decodeJSONLimit(w, r, &in, 8*agent.MaxInstructions) { // room for JSON's escapes
		return
	}
	if len(in.Text) > agent.MaxInstructions || !utf8.ValidString(in.Text) {
		fail(w, http.StatusBadRequest, "SETTINGS_INPUT", "the instructions are UTF-8 text of at most "+strconv.Itoa(agent.MaxInstructions>>10)+" KiB")
		return
	}
	c.keepMu.Lock()
	defer c.keepMu.Unlock()
	if c.settings == nil || c.settings.path == "" {
		fail(w, http.StatusConflict, "SETTINGS_NO_FILE", "serve runs without a configuration directory: there is no ringil.yaml to name the instructions in")
		return
	}
	if err := c.writeInstructions(in.Text); err != nil {
		fail(w, http.StatusInternalServerError, "SETTINGS_INSTRUCTIONS", describe(err))
		return
	}
	c.settings.signal()
	w.WriteHeader(http.StatusNoContent)
}

// writeInstructions writes the text to the user's file, and the first
// time names it in ringil.yaml; the caller holds keepMu.
func (c *cli) writeInstructions(text string) error {
	path, named, err := c.instructionsPath()
	if err != nil {
		return err
	}
	if err := writePrivateFile(path, []byte(text)); err != nil {
		return err
	}
	if named {
		return nil
	}
	return c.changeLocked(func(next *config) { next.Instructions = path }, func(ed *edit) { ed.set("instructions", path) })
}

// changeLocked makes one change to ringil.yaml on the file as it is on
// disk, checked as every change is (see change); the caller holds keepMu.
func (c *cli) changeLocked(mutate func(next *config), apply func(*edit)) error {
	cur, err := c.settings.onDisk(c)
	if err != nil {
		return errors.Wrap(err, ErrUsage, "ringil.yaml on disk is not valid, fix it first")
	}
	next := cur.clone()
	mutate(next)
	if err := next.Validate(); err != nil {
		return err
	}
	if err := c.settings.acceptable(next); err != nil {
		return err
	}
	return writeSettings(c.settings.path, apply)
}

// writePrivateFile writes data to path whole, private to the user.
func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errors.Wrap(err, ErrUsage, "cannot create "+filepath.Dir(path))
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+rand.Text()[:8]+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil { // #nosec G703 -- beside the user's own file
		return errors.Wrap(err, ErrUsage, "cannot write "+path)
	}
	if err := os.Rename(tmp, path); err != nil { // #nosec G703 -- as above
		_ = os.Remove(tmp) // #nosec G703 -- as above
		return errors.Wrap(err, ErrUsage, "cannot write "+path)
	}
	return nil
}

// Where the system has no keyring (a container, a headless machine)
// ringil keeps the app's keys in the encrypted file, in a private folder
// of its own (<config>/ringil/secrets), with a passphrase it makes there:
// neither is ever exported.
const (
	secretsDir = "secrets"
	secretsEnc = "secrets.enc"
	keyFile    = "secrets.key"
	// appPassphrase names that passphrase: a name of its own, which only
	// the key file answers (an environment's RINGIL_SECRETS_PASSPHRASE,
	// of another store, never opens or locks this one).
	appPassphrase = "RINGIL_APP_SECRETS_PASSPHRASE" // #nosec G101 -- the name of a variable, not a value
)

// appStore is the secrets store for a key from the app, under keepMu:
// the one ringil.yaml on disk names (the watcher's copy lags a moment
// behind a first key), else one set up for it (defaultStore).
func (c *cli) appStore(ctx context.Context, base secrets.Resolver) (secrets.Store, error) {
	if c.settings == nil || c.settings.path == "" {
		return c.keyStore(ctx, base)
	}
	cur, err := c.settings.onDisk(c)
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "ringil.yaml on disk is not valid, fix it first")
	}
	if cur.Secrets.Store != "" {
		root, err := c.stateRoot()
		if err != nil {
			return nil, err
		}
		return c.secrets.Store(ctx, cur.Secrets.store(root), base)
	}
	return c.defaultStore(ctx, base)
}

// defaultStore sets up a secrets store for the app's first key: the
// system's keyring when it works, else the encrypted file with a
// passphrase of its own; ringil.yaml then names it, checked as every
// change is. The caller holds keepMu.
func (c *cli) defaultStore(ctx context.Context, base secrets.Resolver) (secrets.Store, error) {
	root, err := c.stateRoot()
	if err != nil {
		return nil, err
	}
	st, section, err := c.keyringStore(ctx, root, base)
	if err != nil {
		st, section, err = c.fileStore(ctx, root, base)
	}
	if err != nil {
		return nil, err
	}
	err = c.changeLocked(func(next *config) { next.Secrets = section }, func(ed *edit) { ed.set("secrets", section.yaml()) })
	if err != nil {
		return nil, err
	}
	c.settings.signal()
	return st, nil
}

// keyringStore is the system's keyring, when it works.
func (c *cli) keyringStore(ctx context.Context, root string, base secrets.Resolver) (secrets.Store, secretsConfig, error) {
	section := secretsConfig{Store: store.Keyring}
	if c.noKeyring {
		return nil, section, errors.New(ErrUsage, "no keyring")
	}
	st, err := c.secrets.Store(ctx, section.store(root), base)
	if err == nil {
		err = probeStore(ctx, st)
	}
	return st, section, err
}

// fileStore is the encrypted file in its private folder, with the
// passphrase ringil makes there.
func (c *cli) fileStore(ctx context.Context, root string, base secrets.Resolver) (secrets.Store, secretsConfig, error) {
	dir := filepath.Join(root, secretsDir)
	section := secretsConfig{Store: store.File, File: filepath.Join(dir, secretsEnc), PassphraseEnv: appPassphrase}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, section, errors.Wrap(err, ErrUsage, "cannot create "+dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a directory: 0700 is private
		return nil, section, errors.Wrap(err, ErrUsage, "cannot make "+dir+" private")
	}
	if err := ensureKeyFile(filepath.Join(dir, keyFile)); err != nil {
		return nil, section, err
	}
	base, err := c.withKeyFile(base)
	if err != nil {
		return nil, section, err
	}
	st, err := c.secrets.Store(ctx, section.store(root), base)
	return st, section, err
}

// probeStore writes and removes a value: a keyring that is there in name only
// (no session bus) fails here.
func probeStore(ctx context.Context, st secrets.Store) error {
	if st == nil {
		return errors.New(ErrUsage, "no store")
	}
	if err := st.Set(ctx, "RINGIL_STORE_PROBE", "ok"); err != nil {
		return err
	}
	_ = st.Delete(ctx, "RINGIL_STORE_PROBE") // it wrote: the keyring works; a probe left behind is harmless
	return nil
}

// ensureKeyFile makes the passphrase file once.
func ensureKeyFile(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand does not fail
	return writePrivateFile(path, []byte(appPassphrase+"="+hex.EncodeToString(b)+"\n"))
}

// withKeyFile is base with the passphrase file, when there is one, after
// the environment and the --env-file.
func (c *cli) withKeyFile(base secrets.Resolver) (secrets.Resolver, error) {
	root, err := c.stateRoot()
	if err != nil {
		return base, nil
	}
	path := filepath.Join(root, secretsDir, keyFile)
	if _, err := os.Lstat(path); err != nil {
		return base, nil
	}
	file, err := secrets.EnvFile(path)
	if err != nil {
		return nil, err
	}
	return secrets.Chain(only{file, appPassphrase}, base), nil // first, and for its own name alone
}

// only answers one name from r, and nothing else.
type only struct {
	r    secrets.Resolver
	name string
}

func (o only) Lookup(ctx context.Context, name string) (string, bool, error) {
	if name != o.name {
		return "", false, nil
	}
	return o.r.Lookup(ctx, name)
}
