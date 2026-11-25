// testhome.go: a temporary home for the tests
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package testhome points every place dirs finds at a temporary home, so
// the defaults the SDK takes write there, never in the user's.
package testhome

import (
	"os"
	"testing"
)

// Main runs the tests of a package in a temporary home.
func Main(m *testing.M) {
	home, err := os.MkdirTemp("", "sdk-home-")
	if err != nil {
		panic(err)
	}
	for _, k := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "AppData", "LocalAppData"} {
		_ = os.Setenv(k, home)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
