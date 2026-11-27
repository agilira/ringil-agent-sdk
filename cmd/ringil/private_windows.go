// private_windows.go: files only the user owns and writes, on Windows
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build windows

package main

import "os"

// noFollow: Windows has no O_NOFOLLOW; os.Root keeps any path inside the
// directory.
const noFollow = 0

// private: permissions on Windows are ACLs, inherited from the user profile
// for the default locations; not checked here.
func private(os.FileInfo) bool { return true }
