// secrets.go: resolving credentials without exposing their sources
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package secrets resolves named credentials (API keys, tokens) for the
// host. Sources are pluggable [Resolver]s: the process environment, a private
// env file, and adapters in sub-packages (Vault, encrypted file, OS keyring).
//
// The model never receives a resolver. Errors never carry a value, and never
// the message of a backend, which may echo one. A failing source stops the
// lookup: there is no silent fallback to the next source.
package secrets

import (
	"context"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

// Error codes.
const (
	// ErrName: the reference is not a valid name.
	ErrName errors.ErrorCode = "SECRET_NAME"
	// ErrLookup: a configured source failed.
	ErrLookup errors.ErrorCode = "SECRET_LOOKUP"
	// ErrMissing: a required credential is absent or empty.
	ErrMissing errors.ErrorCode = "SECRET_MISSING"
	// ErrValue: a resolved value is not acceptable.
	ErrValue errors.ErrorCode = "SECRET_VALUE"
	// ErrSource: a source could not be loaded (unsafe file, bad syntax).
	ErrSource errors.ErrorCode = "SECRET_SOURCE"
)

// maxValue bounds a resolved value; far above any real credential.
const maxValue = 64 << 10

// Resolver looks up a secret by name. found distinguishes an absent name
// from one set to the empty string.
type Resolver interface {
	Lookup(ctx context.Context, name string) (value string, found bool, err error)
}

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// ValidateName accepts environment-style names: a letter or underscore,
// then letters, digits and underscores, up to 128 characters.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New(ErrName, "secret names are letters, digits and _, not starting with a digit")
	}
	return nil
}

// Environment resolves names from the process environment.
type Environment struct{}

// Lookup implements [Resolver].
func (Environment) Lookup(ctx context.Context, name string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if err := ValidateName(name); err != nil {
		return "", false, err
	}
	value, ok := os.LookupEnv(name)
	return value, ok, nil
}

// Map resolves names from a fixed map, e.g. values the host already holds.
type Map map[string]string

// Lookup implements [Resolver].
func (m Map) Lookup(ctx context.Context, name string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if err := ValidateName(name); err != nil {
		return "", false, err
	}
	value, ok := m[name]
	return value, ok, nil
}

// Chain asks each resolver in order; the first that has the name wins. An
// error from any of them ends the lookup: falling back to the next source
// after a failure could silently pick a stale or attacker-planted value.
func Chain(resolvers ...Resolver) Resolver { return chain(resolvers) }

type chain []Resolver

func (c chain) Lookup(ctx context.Context, name string) (string, bool, error) {
	for _, r := range c {
		value, found, err := r.Lookup(ctx, name)
		if err != nil || found {
			return value, found, err
		}
	}
	return "", false, nil
}

// Lookup resolves name through r and checks the value. Backend errors are
// replaced by a code: their text may contain the secret or its location.
func Lookup(ctx context.Context, r Resolver, name string) (string, bool, error) {
	if err := ValidateName(name); err != nil {
		return "", false, err
	}
	value, found, err := r.Lookup(ctx, name)
	switch {
	case err != nil && ctx.Err() != nil:
		return "", false, ctx.Err()
	case errors.HasCode(err, ErrName):
		return "", false, err
	case err != nil:
		return "", false, errors.New(ErrLookup, "secret source failed for "+name)
	case !found:
		return "", false, nil
	}
	if err := checkValue(name, value); err != nil {
		return "", false, err
	}
	return value, true, nil
}

func checkValue(name, value string) error {
	if len(value) > maxValue || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return errors.New(ErrValue, "secret "+name+" has an invalid value")
	}
	return nil
}

// Credential resolves a required credential: present, not empty, and made
// of visible ASCII only, so it can go into an HTTP header as is.
func Credential(ctx context.Context, r Resolver, name string) (string, error) {
	value, found, err := Lookup(ctx, r, name)
	if err != nil {
		return "", err
	}
	if !found || value == "" {
		return "", errors.New(ErrMissing, "credential "+name+" is not set")
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return "", errors.New(ErrValue, "credential "+name+" must be visible ASCII without spaces")
		}
	}
	return value, nil
}

// KeyFunc returns a function that resolves name on every call, for
// configurations that take one (openai.Config.KeyFunc). A rotated
// credential is picked up without restarting.
func KeyFunc(r Resolver, name string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) { return Credential(ctx, r, name) }
}

// OptionalKeyFunc is KeyFunc for a credential that may be absent, as with
// local model servers: absent or empty resolves to "", anything else must
// pass the same checks as Credential.
func OptionalKeyFunc(r Resolver, name string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		value, found, err := Lookup(ctx, r, name)
		if err != nil || !found || value == "" {
			return "", err
		}
		return Credential(ctx, Map{name: value}, name)
	}
}
