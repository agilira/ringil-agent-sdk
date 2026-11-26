// bombs_test.go: small files that would unpack or parse into gigabytes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package document

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A tag of millions of attributes is refused before it is parsed.
func TestXMLAttributeBomb(t *testing.T) {
	attrs := strings.Repeat(` a=""`, (maxTag/5)+10)
	parts := map[string]string{
		"[Content_Types].xml": `<Types/>`,
		"word/document.xml":   `<w:document xmlns:w="w"><w:body><w:p` + attrs + `/></w:body></w:document>`,
	}
	if _, err := read(t, "bomb.docx", parts, Limits{}); err == nil || !strings.Contains(err.Error(), "tag longer") {
		t.Fatalf("%v", err)
	}
	if !tagsFit([]byte(`<a b="` + strings.Repeat("x", 1000) + `">` + strings.Repeat("text", 1<<19) + `</a>`)) {
		t.Fatal("long text between tags refused")
	}
}

// A row of every column naming one long shared string is not held whole.
func TestRowHeldBounded(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	d, err := read(t, "wide.xlsx", bigXLSXParts(maxColumns), Limits{})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 400<<20 {
		t.Fatalf("allocated %d MiB for one row", grew>>20)
	}
	if len(d.Text) > 16<<20+128 {
		t.Fatalf("text %d", len(d.Text))
	}
}

// pdfBomb is a PDF of pages that all show one stream decoding to size bytes.
func pdfBomb(pages, size int) []byte {
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	chunk := []byte(strings.Repeat("0 0 m ", 1024))
	for n := 0; n < size; n += len(chunk) {
		_, _ = zw.Write(chunk)
	}
	_ = zw.Close()
	var b bytes.Buffer
	var offs []int
	obj := func(body string) {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offs), body)
	}
	b.WriteString("%PDF-1.4\n")
	kids := make([]string, pages)
	for i := range kids {
		kids[i] = fmt.Sprintf("%d 0 R", 4+i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages))
	offs = append(offs, b.Len())
	fmt.Fprintf(&b, "3 0 obj\n<< /Length %d /Filter /FlateDecode >>\nstream\n", z.Len())
	b.Write(z.Bytes())
	b.WriteString("\nendstream\nendobj\n")
	for range pages {
		obj("<< /Type /Page /Parent 2 0 R /Contents 3 0 R /Resources << >> >>")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	return b.Bytes()
}

// A small PDF whose pages all decode one large stream stops at the bound,
// quickly, and says which pages it did not read.
func TestPDFDecodeBomb(t *testing.T) {
	start := time.Now()
	d, err := readPDFBytes(pdfBomb(200, 4<<20), Limits{Decoded: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %v", took)
	}
	if !strings.Contains(d.Text, "decodes to more than 8 MiB") || !strings.Contains(d.Text, "to 200 are not read") {
		t.Fatalf("%q", d.Text)
	}
}
