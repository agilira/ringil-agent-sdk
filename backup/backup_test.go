// backup_test.go: exports that verify, and imports all or nothing
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

func TestExportVerifyImport(t *testing.T) {
	mem := tree(t, map[string]string{"memory.json": `{"x":1}`, "memory.lock": ""})
	notes := tree(t, map[string]string{"p/INDEX.md": "# notes", "p/sub/a.md": "a"})
	conf := filepath.Join(tree(t, map[string]string{"ringil.yaml": "models: [x:y]\n"}), "ringil.yaml")
	if err := os.Symlink("/etc/passwd", filepath.Join(notes, "link")); err != nil {
		t.Skip("no symlinks")
	}
	sources := map[string]Source{"memory": {Path: mem}, "notes": {Path: notes}, "config": {Path: conf, File: true}, "missing": {Path: filepath.Join(mem, "nope")}}
	var buf bytes.Buffer
	m, err := Create(bg, &buf, sources, nil)
	if err != nil || len(m.Files) != 4 || m.Files["config"] == "" {
		t.Fatalf("create %v %v", m, err)
	}
	if _, err := Verify(bg, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	checkImport(t, buf.Bytes(), sources)
}

// checkImport changes the state, then imports the archive over it: the
// state is the archive's again, a source it lacks is kept, nothing is
// left beside.
func checkImport(t *testing.T, archive []byte, sources map[string]Source) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(sources["notes"].Path, "p", "sub", "a.md"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sources["notes"].Path, "new.md"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sources["config"].Path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := tree(t, map[string]string{"t.json": "{}"})
	sources["triggers"] = Source{Path: extra}
	got, err := Import(bg, bytes.NewReader(archive), Options{Sources: sources, LockDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	checkImported(t, sources)
	if len(got.Kept) != 2 || got.Kept[1] != "triggers" {
		t.Fatalf("kept %v", got.Kept)
	}
}

// checkImported: the state is the archive's, and nothing is left beside.
func checkImported(t *testing.T, sources map[string]Source) {
	t.Helper()
	if b, _ := os.ReadFile(filepath.Join(sources["notes"].Path, "p", "sub", "a.md")); string(b) != "a" {
		t.Fatalf("notes %q", b)
	}
	if _, err := os.Stat(filepath.Join(sources["notes"].Path, "new.md")); !os.IsNotExist(err) {
		t.Fatal("a file made after the export survived the import")
	}
	if b, _ := os.ReadFile(sources["config"].Path); string(b) != "models: [x:y]\n" {
		t.Fatalf("config %q", b)
	}
	for _, src := range []string{sources["notes"].Path, sources["config"].Path} {
		if left, _ := filepath.Glob(filepath.Join(filepath.Dir(src), ".*-*")); len(left) != 0 {
			t.Fatalf("left beside: %v", left)
		}
	}
}

// A process using the state keeps an import off; once it is gone the
// import runs.
func TestImportWaitsForNoUser(t *testing.T) {
	src := tree(t, map[string]string{"a.txt": "a"})
	var buf bytes.Buffer
	if _, err := Create(bg, &buf, map[string]Source{"data": {Path: src}}, nil); err != nil {
		t.Fatal(err)
	}
	lockDir := t.TempDir()
	release, err := Use(lockDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Import(bg, bytes.NewReader(buf.Bytes()), Options{Sources: map[string]Source{"data": {Path: src}}, LockDir: lockDir}); !errors.HasCode(err, ErrBusy) {
		t.Fatalf("imported under a user: %v", err)
	}
	second, err := Use(lockDir)
	if err != nil {
		t.Fatalf("two users at once: %v", err)
	}
	_ = second()
	_ = release()
	if _, err := Import(bg, bytes.NewReader(buf.Bytes()), Options{Sources: map[string]Source{"data": {Path: src}}, LockDir: lockDir}); err != nil {
		t.Fatal(err)
	}
}

// An archive of a source the host does not name is refused, the state
// untouched.
func TestImportRefusesUnknownSources(t *testing.T) {
	src := tree(t, map[string]string{"a.txt": "a"})
	var buf bytes.Buffer
	if _, err := Create(bg, &buf, map[string]Source{"future": {Path: src}}, nil); err != nil {
		t.Fatal(err)
	}
	mine := tree(t, map[string]string{"b.txt": "b"})
	if _, err := Import(bg, bytes.NewReader(buf.Bytes()), Options{Sources: map[string]Source{"data": {Path: mine}}, LockDir: t.TempDir()}); !errors.HasCode(err, ErrInput) {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(mine, "b.txt")); string(b) != "b" {
		t.Fatal("the state changed")
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
		"escape":    forged(t, map[string]string{"a/\x1b]0;x\a": "x"}, `{"version":1,"files":{}}`),
	}
	for name, data := range cases {
		assertImportDamagedClean(t, name, data)
	}
}
