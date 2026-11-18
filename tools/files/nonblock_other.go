// nonblock_other.go: platforms without O_NONBLOCK on open
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build !unix

package files

// nonBlock: Windows has no FIFOs in the file system namespace.
const nonBlock = 0
