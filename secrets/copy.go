// copy.go: moving secrets from one store to another, checked
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"context"
	"crypto/subtle"
	"slices"

	"github.com/agilira/go-errors"
)

// ErrCopy: a secret did not read back from the new store as it was
// written.
const ErrCopy errors.ErrorCode = "SECRET_COPY"

// Copied is what [Copy] did: the names written and read back, and those
// the source did not have.
type Copied struct {
	Names   []string
	Missing []string
}

// Copy writes the named secrets of from into to, and reads each back from
// to: a value that does not come back the same is an error, before the
// next name. It deletes nothing; removing them from the old store is the
// caller's step, once the copy is done. Names are checked before anything
// is written, and sorted; a name from does not have is reported Missing,
// not an error. Run again after a failure it is safe: Set replaces.
//
// to is read back with its own Lookup: give it a store that reads its
// backend (store.Fresh for a cached one), or the check reads the cache.
// Errors name the secret, never its value.
func Copy(ctx context.Context, from Resolver, to Store, names []string) (Copied, error) {
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			return Copied{}, err
		}
	}
	var done Copied
	for _, name := range names {
		found, err := copyOne(ctx, from, to, name)
		if err != nil {
			return done, err
		}
		if found {
			done.Names = append(done.Names, name)
		} else {
			done.Missing = append(done.Missing, name)
		}
	}
	return done, nil
}

// copyOne copies name, if from has it, and checks it in to.
func copyOne(ctx context.Context, from Resolver, to Store, name string) (bool, error) {
	value, found, err := from.Lookup(ctx, name)
	if err != nil || !found {
		return false, err
	}
	if err := to.Set(ctx, name, value); err != nil {
		return false, err
	}
	back, ok, err := to.Lookup(ctx, name)
	if err != nil {
		return false, err
	}
	if !ok || subtle.ConstantTimeCompare([]byte(back), []byte(value)) != 1 {
		return false, errors.New(ErrCopy, name+" did not read back from the new store as it was written")
	}
	return true, nil
}
