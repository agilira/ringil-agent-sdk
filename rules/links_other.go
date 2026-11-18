// links_other.go: platforms where the link count is not checked
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build !unix

package rules

import "os"

func singleName(os.FileInfo) bool { return true }
