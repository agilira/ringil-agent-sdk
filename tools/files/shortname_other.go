// shortname_other.go: no short names outside Windows
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build !windows

package files

func shortName(string) error { return nil }
