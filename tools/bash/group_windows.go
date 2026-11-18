// group_windows.go: stopping a host command
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build windows

package bash

import "os/exec"

// group keeps the default cancel, which kills the command itself.
func group(*exec.Cmd) {}
