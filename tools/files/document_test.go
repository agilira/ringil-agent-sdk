// document_test.go: tests for read_document
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docx is a Word document with one paragraph per line given.
func docx(t *testing.T, lines ...string) string {
	t.Helper()
	var body strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&body, "<w:p><w:r><w:t>%s</w:t></w:r></w:p>", l)
	}
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	f, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(f, `<w:document xmlns:w="w"><w:body>%s</w:body></w:document>`, body.String()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestReadDocumentPages(t *testing.T) {
	dir := tree(t, map[string]string{"report.docx": docx(t, "one", "two", "three")})
	r := open(t, Config{Dirs: []string{dir}})
	out := mustCall(t, r, "read_document", map[string]any{"path": "report.docx", "offset": 2, "limit": 1})
	if !strings.Contains(out, "report.docx (Word document): lines 2-2 of 3 (continue with offset 3)") || !strings.Contains(out, "2\ttwo") || strings.Contains(out, "one") {
		t.Fatalf("%q", out)
	}
}

// The same policy as read_file: grants, exclusions, no directories.
func TestReadDocumentPolicy(t *testing.T) {
	outside := tree(t, map[string]string{"x.docx": docx(t, "secret")})
	dir := tree(t, map[string]string{".ssh/keys.docx": docx(t, "k"), "d.docx/inner": "", "old.doc": "x", "notes.txt": "x", "fake.xlsx": "not a zip"})
	r := open(t, Config{Dirs: []string{dir}})
	failCall(t, r, "read_document", map[string]any{"path": outside + "/x.docx"}, ErrOutside)
	failCall(t, r, "read_document", map[string]any{"path": ".ssh/keys.docx"}, ErrExcluded)
	failCall(t, r, "read_document", map[string]any{"path": "d.docx"}, ErrInput)
	if out := failCall(t, r, "read_document", map[string]any{"path": "old.doc"}, ErrDocument); !strings.Contains(out, ".docx") {
		t.Errorf("old: %q", out)
	}
	if out := failCall(t, r, "read_document", map[string]any{"path": "notes.txt"}, ErrDocument); !strings.Contains(out, "read_file") {
		t.Errorf("text: %q", out)
	}
	if out := failCall(t, r, "read_document", map[string]any{"path": "fake.xlsx"}, ErrDocument); !strings.Contains(out, "not a zip") {
		t.Errorf("fake: %q", out)
	}
}

func TestReadDocumentSymlinkNoExt(t *testing.T) {
	dir := tree(t, map[string]string{"blobs/abc": docx(t, "linked")})
	if err := os.Symlink(filepath.Join("blobs", "abc"), filepath.Join(dir, "report.docx")); err != nil {
		t.Skip(err)
	}
	r := open(t, Config{Dirs: []string{dir}})
	out := mustCall(t, r, "read_document", map[string]any{"path": "report.docx"})
	if !strings.Contains(out, "(Word document)") || !strings.Contains(out, "linked") {
		t.Fatalf("got %q", out)
	}
}
