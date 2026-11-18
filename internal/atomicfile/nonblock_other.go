// nonblock_other.go: platforms without FIFOs in the file system
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build !unix

package atomicfile

const nonBlock = 0
