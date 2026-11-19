// lock.go: the store's locks
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"context"
	"os"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

// lockWait bounds how long an edit waits for another: edits are brief.
const lockWait = 10 * time.Second

func checkPrivate(root *os.Root) error {
	info, err := root.Stat(".")
	if err != nil {
		return errors.Wrap(err, ErrStore, "cannot inspect the triggers' directory")
	}
	if err := lockfile.CheckPrivate(info); err != nil {
		return errors.Wrap(err, ErrStore, "the triggers' directory must be private to you")
	}
	return nil
}

// lock waits for the named lock and returns its release.
func (s *Store) lock(name string) (func(), error) {
	f, err := lockfile.Open(s.root, name)
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot open "+name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	if err := lockfile.Lock(ctx, f); err != nil {
		_ = f.Close() // #nosec G104 -- not locked
		return nil, errors.Wrap(err, ErrStore, "the triggers is being edited elsewhere")
	}
	return func() {
		_ = lockfile.Unlock(f) // #nosec G104 -- closing releases it too
		_ = f.Close()          // #nosec G104 -- as above
	}, nil
}

// claim takes the runner's lock for as long as it runs: two runners
// on one store would run every trigger twice.
func (s *Store) claim() (func(), error) {
	f, err := lockfile.Open(s.root, lockName)
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot open "+lockName)
	}
	ok, err := lockfile.TryLock(f)
	if err != nil || !ok {
		_ = f.Close() // #nosec G104 -- not ours
		return nil, errors.New(ErrBusy, "another runner is running these triggers")
	}
	return func() {
		_ = lockfile.Unlock(f) // #nosec G104 -- closing releases it too
		_ = f.Close()          // #nosec G104 -- as above
	}, nil
}
