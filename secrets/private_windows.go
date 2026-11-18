// private_windows.go: ownership checks for secret files on Windows
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build windows

package secrets

import "os"

// noFollow: Windows has no O_NOFOLLOW; readPrivate refuses links with
// Lstat before opening.
const noFollow = 0

// checkOwner on Windows checks nothing more: access is governed by ACLs,
// which for files under the user profile are private by default.
func checkOwner(os.FileInfo) error { return nil }
