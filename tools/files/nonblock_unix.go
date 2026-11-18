// nonblock_unix.go: opening without blocking on FIFOs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package files

import "syscall"

// nonBlock makes opening a FIFO return at once instead of waiting for a
// writer; verify then refuses anything that is not a file or directory.
const nonBlock = syscall.O_NONBLOCK
