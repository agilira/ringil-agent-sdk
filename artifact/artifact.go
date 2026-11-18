// artifact.go: durable results agents publish and read back
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package artifact keeps the results agents produce — a report, a table,
// a generated file — so that other tasks, later sessions or the user can
// use them. An artifact has a name within a scope (a project, a task, a
// workflow run, chosen by the host) and versions: publishing a name again
// adds a version, it never overwrites one. Every version is stored by the
// SHA-256 of its bytes, checked on every read.
//
// The model reaches a store through Tools: list, read and, when the host
// allows it, publish. Contents are data, never instructions or grants.
package artifact

import (
	"context"
	"regexp"
	"time"

	"github.com/agilira/go-errors"
)

// Error codes.
const (
	ErrInput    errors.ErrorCode = "ARTIFACT_INPUT"
	ErrNotFound errors.ErrorCode = "ARTIFACT_NOT_FOUND"
	ErrIO       errors.ErrorCode = "ARTIFACT_IO"
	ErrDamaged  errors.ErrorCode = "ARTIFACT_DAMAGED"
)

// Info describes one version of an artifact.
type Info struct {
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	MediaType string    `json:"media_type"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	Created   time.Time `json:"created"`
	// By names who published it: a task, a workflow step, the user.
	By string `json:"by,omitempty"`
}

// Store keeps artifacts by scope.
type Store interface {
	// Put adds a version of name to scope and returns its description.
	Put(ctx context.Context, scope, name, mediaType, by string, data []byte) (Info, error)
	// Get returns a version of name; version 0 is the latest.
	Get(ctx context.Context, scope, name string, version int) (Info, []byte, error)
	// List returns the latest version of every artifact of scope.
	List(ctx context.Context, scope string) ([]Info, error)
	// Delete removes every version of name.
	Delete(ctx context.Context, scope, name string) error
}

var (
	validName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	validScope = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,99}$`)
	validMedia = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9.+-]+$`)
)

func check(scope, name string) error {
	switch {
	case !validScope.MatchString(scope):
		return errors.New(ErrInput, "scopes are 1-100 letters, digits, . _ -, not starting with a dot")
	case !validName.MatchString(name):
		return errors.New(ErrInput, "artifact names are 1-100 letters, digits, . _ -, starting with a letter or digit").WithUserMessage("Artifact names are 1-100 letters, digits, . _ -.")
	}
	return nil
}
