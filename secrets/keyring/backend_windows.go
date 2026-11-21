//go:build windows

// backend_windows.go: the Windows Credential Manager
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package keyring

import (
	stderrors "errors"

	"github.com/danieljoos/wincred"
)

// maxBlob is the Credential Manager's bound on a value
// (CRED_MAX_CREDENTIAL_BLOB_SIZE).
const maxBlob = 5 * 512

type credentials struct{}

func system() backend { return credentials{} }

// target is the entry's name in the Credential Manager, which ignores
// case: both TargetName (carrying service) and UserName (exact name)
// are checked on every call.
func target(service, name string) string { return service + ":" + name }

func sameEntry(c *wincred.GenericCredential, service, name string) bool {
	return c.UserName == name && c.TargetName == target(service, name)
}

// read is the entry for service and name, if there is one under any
// case.
func read(service, name string) (*wincred.GenericCredential, error) {
	c, err := wincred.GetGenericCredential(target(service, name))
	if stderrors.Is(err, wincred.ErrElementNotFound) {
		return nil, nil
	}
	return c, err
}

func (credentials) get(service, name string) (string, bool, error) {
	c, err := read(service, name)
	if err != nil || c == nil || !sameEntry(c, service, name) {
		return "", false, err // another service or name differing in case is not this one
	}
	return string(c.CredentialBlob), true, nil
}

func (credentials) set(service, name, value string) error {
	if len(value) > maxBlob {
		return errTooBig
	}
	old, err := read(service, name)
	if err != nil {
		return err
	}
	if old != nil && !sameEntry(old, service, name) {
		return errCaseClash // it would replace the other one
	}
	c := wincred.NewGenericCredential(target(service, name))
	c.UserName = name
	c.CredentialBlob = []byte(value)
	c.Persist = wincred.PersistLocalMachine
	return c.Write()
}

func (credentials) del(service, name string) error {
	c, err := read(service, name)
	if err != nil || c == nil || !sameEntry(c, service, name) {
		return err
	}
	return c.Delete()
}
