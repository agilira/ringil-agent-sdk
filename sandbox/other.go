// other.go: platforms without a sandbox backend yet
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package sandbox

import "github.com/agilira/go-errors"

// newPlatform: macOS (seatbelt) and Windows (AppContainer) backends are
// planned; until then the sandbox says so instead of running unconfined.
func newPlatform() (Sandbox, error) {
	return nil, errors.New(ErrUnavailable, "no sandbox backend on this platform yet")
}
