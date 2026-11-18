// backup_test.go: archives that verify, and restores that stay apart
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agilira/go-errors"
)

var bg = context.Background()

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBackupVerifyRestore(t *testing.T) {
	mem := tree(t, map[string]string{"memory.json": `{"x":1}`, "memory.lock": ""})
	notes := tree(t, map[string]string{"p/INDEX.md": "# notes", "p/sub/a.md": "a"})
	if err := os.Symlink("/etc/passwd", filepath.Join(notes, "link")); err != nil {
		t.Skip("no symlinks")
	}
	var buf bytes.Buffer
	m, err := Create(bg, &buf, map[string]string{"memory": mem, "notes": notes, "missing": filepath.Join(mem, "nope")}, nil)
	if err != nil || len(m.Files) != 3 {
		t.Fatalf("create %v %v", m, err)
	}
	if _, err := Verify(bg, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	checkRestore(t, buf.Bytes())
}

func checkRestore(t *testing.T, archive []byte) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(bg, bytes.NewReader(archive), dest); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "notes", "p", "sub", "a.md")); err != nil || string(got) != "a" {
		t.Fatalf("restored %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dest, QuarantineFile)); err != nil {
		t.Fatal("no quarantine mark")
	}
	if _, err := Restore(bg, bytes.NewReader(archive), dest); !errors.HasCode(err, ErrExists) {
		t.Fatalf("restore over an existing directory: %v", err)
	}
}

// forged writes an archive by hand.
func forged(t *testing.T, entries map[string]string, manifest string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBadArchivesAreRefused(t *testing.T) {
	cases := map[string][]byte{
		"traversal": forged(t, map[string]string{"../evil": "x"}, `{"version":1,"files":{}}`),
		"absolute":  forged(t, map[string]string{"/etc/x": "x"}, `{"version":1,"files":{}}`),
		"digest":    forged(t, map[string]string{"a/b": "x"}, `{"version":1,"files":{"a/b":"00"}}`),
		"extra":     forged(t, map[string]string{"a/b": "x"}, `{"version":1,"files":{}}`),
		"garbage":   []byte("not an archive"),
	}
	for name, data := range cases {
		dest := filepath.Join(t.TempDir(), "r")
		if _, err := Restore(bg, bytes.NewReader(data), dest); !errors.HasCode(err, ErrDamaged) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Errorf("%s: something was restored", name)
		}
	}
}
