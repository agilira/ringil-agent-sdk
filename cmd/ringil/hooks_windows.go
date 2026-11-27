// hooks_windows.go: a hook's process, on Windows
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build windows

package main

import "os/exec"

// hookProcess leaves the hook as it is: its timeout kills it, not what it
// started.
func hookProcess(*exec.Cmd) {}

// endGroup has no group to end on Windows: what a hook starts and leaves
// is its own.
func endGroup(*exec.Cmd) {}
