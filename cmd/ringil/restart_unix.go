//go:build !windows

// restart_unix.go: the process starts again, in place
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"os"
	"syscall"
)

// restart replaces the process with the same command, arguments and
// environment: serve after an import. It returns only if that fails.
func restart() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	_ = syscall.Exec(exe, os.Args, os.Environ()) // #nosec G204 G702 -- our own executable, with the arguments it was started with
}
