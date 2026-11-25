// main_test.go: the tests keep the program's places in a temporary home
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agilira/ringil-agent-sdk/dirs"
	"github.com/agilira/ringil-agent-sdk/internal/testhome"
)

func TestMain(m *testing.M) { testhome.Main(m) }

// Left alone, the agent's own places are not read, even inside a granted
// directory: its sessions and settings never reach the model.
func TestOwnPlacesAreNeverRead(t *testing.T) {
	d, err := dirs.Program()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MkdirAll(); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(d.State, "session.json")
	if err := os.WriteFile(secret, []byte("history"), 0o600); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	w, err := New(Config{Dirs: []string{home}})
	if err != nil {
		t.Fatal(err)
	}
	read := w.ReadTools()[0]
	in, _ := json.Marshal(map[string]string{"path": secret})
	if out, err := read.Call(context.Background(), in); err == nil {
		t.Fatalf("read the agent's own state: %q", out.Content)
	}
	// With Own empty and not nil, they are files like the others.
	open, err := New(Config{Dirs: []string{home}, Own: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open.ReadTools()[0].Call(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}
