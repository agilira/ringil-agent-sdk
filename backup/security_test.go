// security_test.go: fuzz and archive integrity/traversal security tests for backup
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

func FuzzVerify(f *testing.F) {
	oldMax := MaxTotal
	MaxTotal = 1 << 20
	f.Cleanup(func() { MaxTotal = oldMax })

	src := f.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600); err != nil {
		f.Fatal(err)
	}
	var validBuf bytes.Buffer
	if _, err := Create(bg, &validBuf, map[string]Source{"data": {Path: src}}, nil); err != nil {
		f.Fatal(err)
	}
	f.Add(validBuf.Bytes())
	f.Add(buildRawArchive([]rawTarEntry{
		{name: "../evil", typ: tar.TypeReg, body: "x"},
		{name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`},
	}))
	f.Add(buildRawArchive([]rawTarEntry{
		{name: "a/b", typ: tar.TypeReg, body: "x"},
		{name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{"a/b":"00"}}`},
	}))
	if len(validBuf.Bytes()) > 10 {
		f.Add(validBuf.Bytes()[:len(validBuf.Bytes())-10])
	}
	f.Add([]byte("not an archive"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			return
		}
		m, err := Verify(bg, bytes.NewReader(data))
		if err == nil {
			checkVerifiedManifest(t, m)
		}
	})
}

func checkVerifiedManifest(t *testing.T, m *Manifest) {
	t.Helper()
	if m == nil || m.Version != 1 || m.Files == nil {
		t.Fatalf("Verify succeeded with invalid manifest: %+v", m)
	}
	for k := range m.Files {
		if !safePath(k) {
			t.Fatalf("Verify accepted unsafe path in manifest: %q", k)
		}
	}
}

type rawTarEntry struct {
	name, link, body string
	typ              byte
}

func buildRawArchive(entries []rawTarEntry) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		_ = tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Linkname: e.link,
			Mode:     0o600,
			Size:     int64(len(e.body)),
			Typeflag: e.typ,
		})
		if len(e.body) > 0 {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func mustRawArchive(t *testing.T, entries []rawTarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Linkname: e.link,
			Mode:     0o600,
			Size:     int64(len(e.body)),
			Typeflag: e.typ,
		}); err != nil {
			t.Fatalf("WriteHeader(%q): %v", e.name, err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("Write(%q): %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSecurityTarAttacksRefused(t *testing.T) {
	if safePath("a/\x00b") {
		t.Fatal("safePath accepted NUL byte")
	}
	cases := map[string][]byte{
		"symlink":       mustRawArchive(t, []rawTarEntry{{name: "a/link", typ: tar.TypeSymlink, link: "/etc/passwd"}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"hardlink":      mustRawArchive(t, []rawTarEntry{{name: "a/hard", typ: tar.TypeLink, link: "/etc/passwd"}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"dirEntry":      mustRawArchive(t, []rawTarEntry{{name: "a/dir", typ: tar.TypeDir}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"fifoEntry":     mustRawArchive(t, []rawTarEntry{{name: "a/fifo", typ: tar.TypeFifo}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"dotDotMid":     mustRawArchive(t, []rawTarEntry{{name: "a/../b", typ: tar.TypeReg, body: "x"}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"dotSlash":      mustRawArchive(t, []rawTarEntry{{name: "./a", typ: tar.TypeReg, body: "x"}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"backslash":     mustRawArchive(t, []rawTarEntry{{name: `a\b`, typ: tar.TypeReg, body: "x"}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"dot":           mustRawArchive(t, []rawTarEntry{{name: ".", typ: tar.TypeReg, body: "x"}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"doubleSlash":   mustRawArchive(t, []rawTarEntry{{name: "a//b", typ: tar.TypeReg, body: "x"}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"ctrlChar":      mustRawArchive(t, []rawTarEntry{{name: "a/\x01b", typ: tar.TypeReg, body: "x"}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"newlineChar":   mustRawArchive(t, []rawTarEntry{{name: "a/\nb", typ: tar.TypeReg, body: "x"}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"duplicate":     mustRawArchive(t, []rawTarEntry{{name: "a/b", typ: tar.TypeReg, body: "x"}, {name: "a/b", typ: tar.TypeReg, body: "y"}, {name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}}),
		"afterManifest": mustRawArchive(t, []rawTarEntry{{name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`}, {name: "a/b", typ: tar.TypeReg, body: "x"}}),
		"manifestLink":  mustRawArchive(t, []rawTarEntry{{name: manifestName, typ: tar.TypeSymlink, link: "/etc/passwd"}}),
		"noManifest":    mustRawArchive(t, []rawTarEntry{{name: "a/b", typ: tar.TypeReg, body: "x"}}),
		"badVersion2":   mustRawArchive(t, []rawTarEntry{{name: manifestName, typ: tar.TypeReg, body: `{"version":2,"files":{}}`}}),
		"badVersion0":   mustRawArchive(t, []rawTarEntry{{name: manifestName, typ: tar.TypeReg, body: `{"version":0,"files":{}}`}}),
		"nullFiles":     mustRawArchive(t, []rawTarEntry{{name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":null}`}}),
		"missingFile":   mustRawArchive(t, []rawTarEntry{{name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{"a/b":"00"}}`}}),
	}
	for name, archive := range cases {
		assertImportDamagedClean(t, name, archive)
	}

	oldMax := MaxTotal
	MaxTotal = 1024
	t.Cleanup(func() { MaxTotal = oldMax })
	oversize := mustRawArchive(t, []rawTarEntry{
		{name: "a/big", typ: tar.TypeReg, body: strings.Repeat("x", 1025)},
		{name: manifestName, typ: tar.TypeReg, body: `{"version":1,"files":{}}`},
	})
	assertImportDamagedClean(t, "maxTotal", oversize)
}

// assertImportDamagedClean: a damaged archive is refused, the state as it
// was, nothing left beside it.
func assertImportDamagedClean(t *testing.T, name string, archive []byte) {
	t.Helper()
	parent := t.TempDir()
	state := filepath.Join(parent, "a")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "mine"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(bg, bytes.NewReader(archive), Options{Sources: map[string]Source{"a": {Path: state}}, LockDir: t.TempDir()}); !errors.HasCode(err, ErrDamaged) {
		t.Errorf("%s: got %v, want ErrDamaged", name, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 {
		t.Errorf("%s: leftover beside the state: %v %v", name, entries, err)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "mine")); string(b) != "mine" {
		t.Errorf("%s: the state changed", name)
	}
}

func TestSecurityCreateRejectsBadSourcesAndSkipsLocks(t *testing.T) {
	dir := tree(t, map[string]string{
		"keep.txt": "ok",
		".lock":    "lock",
		"foo.lock": "lock",
		".tmp.tmp": "tmp",
		".abc.tmp": "tmp",
	})
	for _, bad := range []string{"../a", "Upper", "", "a/b", strings.Repeat("a", 33)} {
		var buf bytes.Buffer
		if _, err := Create(bg, &buf, map[string]Source{bad: {Path: dir}}, nil); !errors.HasCode(err, ErrInput) {
			t.Errorf("source %q accepted: %v", bad, err)
		}
	}
	var buf bytes.Buffer
	m, err := Create(bg, &buf, map[string]Source{"data": {Path: dir}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 1 || m.Files["data/keep.txt"] == "" {
		t.Fatalf("unexpected archived files: %+v", m.Files)
	}
}
