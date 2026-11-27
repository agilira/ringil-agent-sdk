// private_test.go: the private-file check
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ACLs, not modes")
	}
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[os.FileMode]bool{0o600: true, 0o644: true, 0o602: false, 0o666: false} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(path)
		if private(info) != want {
			t.Errorf("mode %o: private=%v", mode, !want)
		}
	}
}

func TestLoopback(t *testing.T) {
	for host, want := range map[string]bool{"127.0.0.1": true, "::1": true, "localhost": false, "10.0.0.1": false, "": false} {
		if loopback(host) != want {
			t.Errorf("%q", host)
		}
	}
}
