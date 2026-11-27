// export.go: the agent's state exported to one archive and imported back
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/backup"
)

// stateRoot is where the state lives, and its lock: <config>/ringil.
func (c *cli) stateRoot() (string, error) {
	base, err := c.configDir()
	if err != nil {
		return "", errors.Wrap(err, ErrUsage, "no user config directory")
	}
	return filepath.Join(base, "ringil"), nil
}

// stateSources are what an export holds and an import replaces: the
// conversations, memory, notes, workflows, artifacts, skills, triggers
// (their webhooks' secrets with them), ringil.yaml, AGENTS.md and the
// user's instructions, the
// chat apps' records, and the documents sent in the chats. Left out: the
// secrets store, serve's token and sign-ins (credentials), the audit log
// (a record is never replaced by an older one), caches.
func (c *cli) stateSources(ctx *orpheus.Context) (map[string]backup.Source, error) {
	root, err := c.stateRoot()
	if err != nil {
		return nil, err
	}
	sources := map[string]backup.Source{}
	for _, n := range []string{"memory", "projects", "workflows", "artifacts", "app-artifacts", "scratch", "skills", "triggers"} {
		sources[n] = backup.Source{Path: filepath.Join(root, n)}
	}
	files := map[string]string{"agents": "AGENTS.md"}
	for _, app := range []string{"telegram", "slack"} {
		files[app+"-sessions"], files[app+"-phones"] = app+"-sessions.json", app+"-phones.json"
	}
	for n, f := range files {
		sources[n] = backup.Source{Path: filepath.Join(root, f), File: true}
	}
	sessions, err := c.sessionsDir(ctx)
	if err != nil {
		return nil, err
	}
	sources["sessions"] = backup.Source{Path: sessions}
	if conf, _ := c.configPath(ctx); conf != "" {
		sources["config"] = backup.Source{Path: conf, File: true}
	}
	if own, _, err := c.instructionsPath(); err == nil {
		sources["instructions"] = backup.Source{Path: own, File: true}
	}
	if data, err := c.dataDir(); err == nil {
		sources["inbox"] = backup.Source{Path: filepath.Join(data, "inbox")}
	}
	return sources, nil
}

// useState holds the state's shared lock while the command runs: an
// import waits for no one to use it.
func (c *cli) useState() error {
	root, err := c.stateRoot()
	if err != nil {
		return nil // no state to share
	}
	release, err := backup.Use(root)
	if err != nil {
		return err
	}
	c.closers = append(c.closers, release)
	return nil
}

// holdingState runs a command that writes the state without loading the
// configuration (setup) under the state's lock.
func (c *cli) holdingState(run func(*orpheus.Context) error) func(*orpheus.Context) error {
	return func(ctx *orpheus.Context) error {
		if err := c.useState(); err != nil {
			return err
		}
		defer c.shutdown()
		return run(ctx)
	}
}

// exportTo writes the state's archive to w.
func (c *cli) exportTo(ctx *orpheus.Context, w io.Writer) (*backup.Manifest, error) {
	sources, err := c.stateSources(ctx)
	if err != nil {
		return nil, err
	}
	return backup.Create(ctx.Context(), w, sources, nil)
}

// remoteMemory says where the memory is when an archive cannot hold it.
func (c *cli) remoteMemory() string {
	if k := c.conf().Memory.Kind; k == "mem0" || k == "chroma" {
		return "; the memory is kept by " + k + ", not in the archive"
	}
	return ""
}

func (c *cli) cmdStateExport(ctx *orpheus.Context) error {
	if len(ctx.Positional()) != 1 {
		return errors.New(ErrUsage, "ringil state export <file.tar.gz>")
	}
	path := ctx.Positional()[0]
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the user names the file; never overwritten
	if err != nil {
		return errors.Wrap(err, ErrUsage, "cannot create "+path+" (it must not exist)")
	}
	m, err := c.exportTo(ctx, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if rerr := os.Remove(path); rerr != nil {
			return errors.Wrap(err, ErrUsage, "the export failed and left "+path)
		}
		return err
	}
	return c.line("exported " + strconv.Itoa(len(m.Files)) + " files to " + clean(path) + c.remoteMemory())
}

func (c *cli) cmdStateVerify(ctx *orpheus.Context) error {
	if len(ctx.Positional()) != 1 {
		return errors.New(ErrUsage, "ringil state verify <file.tar.gz>")
	}
	f, err := os.Open(ctx.Positional()[0]) // #nosec G304 -- the user names the file
	if err != nil {
		return errors.Wrap(err, ErrUsage, "cannot open the archive")
	}
	defer closeFile(f)
	m, err := backup.Verify(ctx.Context(), f)
	if err != nil {
		return err
	}
	return c.line("ok: " + strconv.Itoa(len(m.Files)) + " files, exported " + m.Created.Format("2006-01-02 15:04"))
}

// cmdStateImport replaces the state with an archive's, after a copy of
// the state as it is: no ringil may be running (it holds the state).
func (c *cli) cmdStateImport(ctx *orpheus.Context) error {
	if len(ctx.Positional()) != 1 {
		return errors.New(ErrUsage, "ringil state import <file.tar.gz>")
	}
	before, n, err := c.importState(ctx, ctx.Positional()[0])
	if err != nil {
		return err
	}
	return c.line("imported " + strconv.Itoa(n) + " files; the state before is in " + clean(before))
}

// importState checks the archive, then imports it; once the import has
// the state to itself it first exports it as it is into the data
// directory (it outlives updates): the copy's path.
func (c *cli) importState(ctx *orpheus.Context, archive string) (before string, files int, err error) {
	if err := verifyFile(ctx.Context(), archive); err != nil {
		return "", 0, err
	}
	sources, err := c.stateSources(ctx)
	if err != nil {
		return "", 0, err
	}
	root, err := c.stateRoot()
	if err != nil {
		return "", 0, err
	}
	f, err := os.Open(archive) // #nosec G304 -- the user's archive, checked above
	if err != nil {
		return "", 0, errors.Wrap(err, ErrUsage, "cannot open the archive")
	}
	defer closeFile(f)
	got, err := backup.Import(ctx.Context(), f, backup.Options{Sources: sources, LockDir: root, Before: func() error {
		var err error
		before, err = c.safetyCopy(ctx)
		return err
	}})
	if err != nil {
		if before != "" { // the whole of what happened, then where the copy is
			err = errors.Wrap(err, ErrUsage, describe(err)+"; the copy of the state made before the import is in "+before)
		}
		return "", 0, err
	}
	return before, len(got.Manifest.Files), nil
}

func verifyFile(ctx context.Context, archive string) error {
	f, err := os.Open(archive) // #nosec G304 -- the user names the file
	if err != nil {
		return errors.Wrap(err, ErrUsage, "cannot open the archive")
	}
	defer closeFile(f)
	_, err = backup.Verify(ctx, f)
	return err
}

// safetyCopy exports the state as it is before an import, into
// <data>/backups.
func (c *cli) safetyCopy(ctx *orpheus.Context) (string, error) {
	data, err := c.dataDir()
	if err != nil {
		return "", errors.Wrap(err, ErrUsage, "no user data directory for the copy of the state before the import")
	}
	dir := filepath.Join(data, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errors.Wrap(err, ErrUsage, "cannot create "+dir)
	}
	pruneCopies(dir)
	path := filepath.Join(dir, "before-import-"+strings.ReplaceAll(time.Now().Format("20060102-150405.000"), ".", "-")+".tar.gz")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- our own data directory
	if err != nil {
		return "", errors.Wrap(err, ErrUsage, "cannot create "+path)
	}
	_, err = c.exportTo(ctx, f)
	if err == nil {
		err = f.Sync() // on disk before anything changes
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", errors.Wrap(err, ErrUsage, "the copy of the state before the import failed: nothing was imported")
	}
	return path, nil
}

// keptCopies is how many copies made before imports stay.
const keptCopies = 5

// pruneCopies removes the oldest copies past keptCopies, before a new one.
func pruneCopies(dir string) {
	old, _ := filepath.Glob(filepath.Join(dir, "before-import-*.tar.gz"))
	sort.Strings(old) // their names sort by time
	for len(old) >= keptCopies {
		_ = os.Remove(old[0])
		old = old[1:]
	}
}

func (c *cli) cmdStateHelp(*orpheus.Context) error {
	return c.line("ringil state export <file> | verify <file> | import <file>")
}
