// document_test.go: tests for read_document
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
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

// pdfDoc is a PDF with a page per content stream.
func pdfDoc(contents ...string) string {
	var b strings.Builder
	var offs []int
	obj := func(body string) {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offs), body)
	}
	b.WriteString("%PDF-1.4\n")
	kids := make([]string, len(contents))
	for i := range contents {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(contents)))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, c := range contents {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", 5+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(c), c))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	return b.String()
}

// A PDF with a scanned page comes whole with its text, once: on the read
// that shows its first page. A PDF of text alone comes as text.
func TestReadDocumentScans(t *testing.T) {
	text := "BT /F1 12 Tf 72 700 Td (words) Tj ET"
	literal := "BT /F1 12 Tf 72 700 Td (## Page 1) Tj ET"
	dir := tree(t, map[string]string{"scan.pdf": pdfDoc(text, "q Q"), "literal.pdf": pdfDoc("q Q", literal), "text.pdf": pdfDoc(text)})
	r := open(t, Config{Dirs: []string{dir}})
	for _, c := range []struct {
		path   string
		offset int
		want   int
	}{{"scan.pdf", 1, 1}, {"scan.pdf", 4, 0}, {"literal.pdf", 4, 0}, {"text.pdf", 1, 0}} {
		in, _ := json.Marshal(map[string]any{"path": c.path, "offset": c.offset})
		res, err := r.Execute(context.Background(), agent.ToolCall{ID: "1", Name: "read_document", Input: in})
		if err != nil || len(res.Images) != c.want {
			t.Fatalf("%s offset %d: %d attachments, %v (%s)", c.path, c.offset, len(res.Images), err, res.Content)
		}
		if c.want == 1 && (res.Images[0].Type != agent.PartDocument || !strings.Contains(res.Content, "1 pages have no text")) {
			t.Fatalf("%s: %q", c.path, res.Content)
		}
	}
}
