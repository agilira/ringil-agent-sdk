//go:build !(linux || freebsd || netbsd || openbsd || dragonfly || darwin || windows)

// backend_other.go: no keyring on this system
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package keyring

type none struct{}

func system() backend { return none{} }

func (none) get(string, string) (string, bool, error) { return "", false, errNoKeyring }
func (none) set(string, string, string) error         { return errNoKeyring }
func (none) del(string, string) error                 { return errNoKeyring }
