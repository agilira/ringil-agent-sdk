// readfile_test.go: a whole file for a host's tools, under read_file's rules
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

func TestReadFileForHost(t *testing.T) {
	dir := tree(t, map[string]string{"out/sheet.xlsx": "PK\x03\x04data", ".env": "SECRET=1", "out/blob": "\x89PNG\r\n\x1a\nxx"})
	outside := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := New(Config{Dirs: []string{dir}, Own: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, want := range map[string]string{"out/sheet.xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "out/blob": "image/png"} {
		if data, media, err := w.ReadFile(ctx, filepath.Join(dir, name), 1<<20); err != nil || len(data) == 0 || media != want {
			t.Fatalf("%s: %q %v", name, media, err)
		}
	}
	checkReadFileRefusals(t, w, dir, outside)
}

func checkReadFileRefusals(t *testing.T, w Workspace, dir, outside string) {
	t.Helper()
	ctx := context.Background()
	for name, p := range map[string]string{"excluded": filepath.Join(dir, ".env"), "outside": outside, "directory": filepath.Join(dir, "out")} {
		if _, _, err := w.ReadFile(ctx, p, 1<<20); err == nil {
			t.Errorf("%s read", name)
		}
	}
	if _, _, err := w.ReadFile(ctx, filepath.Join(dir, "out/sheet.xlsx"), 4); !errors.HasCode(err, ErrInput) || !strings.Contains(err.Error(), "4 bytes") {
		t.Fatalf("over the bound: %v", err)
	}
	if _, _, err := w.ReadFile(ctx, filepath.Join(dir, "out/sheet.xlsx"), 0); !errors.HasCode(err, ErrInput) {
		t.Fatalf("non-positive max: %v", err)
	}
	if err := os.Symlink(filepath.Join("out", "blob"), filepath.Join(dir, "link.xlsx")); err != nil {
		t.Skip(err)
	}
	if _, media, err := w.ReadFile(ctx, "link.xlsx", 1<<20); err != nil || media != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		t.Fatalf("symlink media type: %q %v", media, err)
	}
}
