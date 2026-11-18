// group_other.go: platforms without process groups
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build !unix

package mcp

import "os/exec"

func ownGroup(*exec.Cmd) {}

// killGroup: on Windows the transport stops the server itself; children
// it started are not tracked.
func killGroup(*exec.Cmd) error { return nil }
