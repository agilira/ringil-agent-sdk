// pdf_test.go: tests for the text of PDF files
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package document

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"
)

// pdfOf makes a PDF with one page per content stream, with a correct
// cross-reference table.
func pdfOf(contents ...string) []byte {
	var b bytes.Buffer
	var offs []int
	obj := func(body string) {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offs), body)
	}
	b.WriteString("%PDF-1.4\n")
	n := len(contents)
	kids := make([]string, n)
	for i := range contents {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, c := range contents {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", 5+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(c), c))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	return b.Bytes()
}

func readPDFBytes(b []byte, l Limits) (Doc, error) {
	return Read(bytes.NewReader(b), int64(len(b)), "x.pdf", l)
}

func TestPDFPages(t *testing.T) {
	d, err := readPDFBytes(pdfOf("BT /F1 12 Tf 72 700 Td (Hello PDF) Tj ET", "BT /F1 12 Tf 72 700 Td (Second page) Tj ET"), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != PDF || !strings.Contains(d.Text, "## Page 1\nHello PDF") || !strings.Contains(d.Text, "## Page 2\nSecond page") {
		t.Fatalf("%q", d.Text)
	}
}

// A page with no text is named a scan; a PDF of scans says so once.
func TestPDFScanned(t *testing.T) {
	d, err := readPDFBytes(pdfOf("q 1 0 0 1 0 0 cm Q", "q Q"), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(d.Text, "no text on this page") != 2 || !strings.Contains(d.Text, "the PDF is scanned") {
		t.Fatalf("%q", d.Text)
	}
}

// A damaged PDF is an error, never a panic; a page that cannot be read
// does not lose the others.
func TestPDFDamaged(t *testing.T) {
	good := pdfOf("BT /F1 12 Tf (ok) Tj ET")
	for name, b := range map[string][]byte{
		"empty":     nil,
		"not a pdf": []byte("hello"),
		"truncated": good[:len(good)/2],
		"bad xref":  bytes.Replace(good, []byte("xref"), []byte("xxxx"), 1),
	} {
		if _, err := readPDFBytes(b, Limits{}); !errors.HasCode(err, ErrFormat) {
			t.Errorf("%s: %v", name, err)
		}
	}
	d, err := readPDFBytes(pdfOf("BT /F1 12 Tf (one) Tj ET", "BT /F1 12 Tf ) Tj TL ' \" ET", "BT /F1 12 Tf (three) Tj ET"), Limits{})
	if err != nil || !strings.Contains(d.Text, "one") || !strings.Contains(d.Text, "three") {
		t.Fatalf("%v %q", err, d.Text)
	}
}

func TestPDFTextLimit(t *testing.T) {
	d, err := readPDFBytes(pdfOf("BT /F1 12 Tf ("+strings.Repeat("x", 500)+") Tj ET"), Limits{Text: 100})
	if err != nil || !d.Cut || len(d.Text) > 200 {
		t.Fatalf("%v %v %d", err, d.Cut, len(d.Text))
	}
}

// FuzzPDF: any bytes as a PDF never panic, never hang, and give UTF-8.
func FuzzPDF(f *testing.F) {
	f.Add(pdfOf("BT /F1 12 Tf (Hello) Tj ET"))
	f.Add(pdfOf("BT /F1 12 Tf [(a) -200 (b)] TJ ET", "q Q"))
	f.Add([]byte("%PDF-1.4\n1 0 obj << /Type /Catalog /Pages 1 0 R >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF"))
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := readPDFBytes(b, Limits{Text: 1 << 16})
		if err == nil {
			check(t, d, 1<<16)
		}
	})
}

// Reference cycles in a crafted PDF made the library loop forever: the
// fork bounds them (agilira/pdf, upstream ledongthuc/pdf#109). A cycle is
// an error or an empty page, in time.
func TestPDFCyclesReturn(t *testing.T) {
	good := pdfOf("BT /F1 12 Tf (ok) Tj ET")
	start := bytes.LastIndex(good, []byte("startxref\n")) + len("startxref\n")
	xref := string(good[start : start+bytes.IndexByte(good[start:], '\n')])
	for name, b := range map[string][]byte{
		"pages list themselves": bytes.Replace(good, []byte("/Kids [4 0 R]"), []byte("/Kids [2 0 R]"), 1),
		"prev points to itself": bytes.Replace(good, []byte("/Root 1 0 R >>"), []byte("/Root 1 0 R /Prev "+xref+" >>"), 1),
		"parent is the page":    bytes.Replace(good, []byte("/Type /Page /Parent 2 0 R"), []byte("/Type /Page /Parent 4 0 R"), 1),
	} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = readPDFBytes(b, Limits{})
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: did not return", name)
		}
	}
}
