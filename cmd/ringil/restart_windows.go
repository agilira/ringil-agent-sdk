// restart_windows.go: the process starts again, as a new one
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"os"
	"os/exec"
)

// restart starts the same command again and ends this one: Windows has
// no exec in place. It returns only if the start fails.
func restart() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, os.Args[1:]...) // #nosec G204 G702 -- our own executable, with the arguments it was started with
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if cmd.Start() == nil {
		os.Exit(exitOK)
	}
}
