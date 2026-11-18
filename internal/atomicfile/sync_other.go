// sync_other.go: platforms where a directory cannot be synced
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build !unix

package atomicfile

import "os"

// syncDir: on Windows a rename is durable once the file was synced.
func syncDir(*os.Root) error { return nil }
