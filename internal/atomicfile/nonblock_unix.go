// nonblock_unix.go: opening without blocking on FIFOs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package atomicfile

import "syscall"

const nonBlock = syscall.O_NONBLOCK
