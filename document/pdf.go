// pdf.go: the text of PDF files
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package document

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/pdf"
)

// maxPages bounds the pages read; past it the text ends with a note.
const maxPages = 2000

// pdfDoc is Read for a PDF.
func pdfDoc(r io.ReaderAt, size int64, limits Limits) (Doc, error) {
	out := &writer{max: limits.withDefaults().Text}
	scans, err := readPDF(r, size, out)
	if err != nil && !errors.HasCode(err, errFull) {
		return Doc{}, err
	}
	return Doc{Kind: PDF, Text: out.String(), Cut: out.cut, Scans: scans}, nil
}

// readPDF: the text of each page, in order, under "## Page N". A scanned
// page has no text: it is named as such, for the host to send the page
// to a model that reads images. The library reports a damaged file by
// panicking: that is turned into an error here (it starts no goroutine).
func readPDF(r io.ReaderAt, size int64, out *writer) (scans int, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = errors.New(ErrFormat, "a damaged PDF ("+short(v)+")")
		}
	}()
	rd, err := openPDF(r, size)
	if err != nil {
		return 0, err
	}
	return writePages(rd, out)
}

func openPDF(r io.ReaderAt, size int64) (*pdf.Reader, error) {
	rd, err := pdf.NewReader(r, size)
	switch {
	case err == nil:
		return rd, nil
	case strings.Contains(err.Error(), "encrypt") || strings.Contains(err.Error(), "password"):
		return nil, errors.New(ErrFormat, "a password-protected PDF: open it, remove the password, and read it again")
	}
	return nil, errors.New(ErrFormat, "not a valid PDF")
}

// writePages writes each page's text, at most maxPages, and counts the
// pages without text.
func writePages(rd *pdf.Reader, out *writer) (int, error) {
	pages, empty := rd.NumPage(), 0
	for n := 1; n <= pages && n <= maxPages; n++ {
		text, err := pageText(rd, n)
		if err != nil {
			text = "[this page could not be read]"
		}
		if strings.TrimSpace(text) == "" {
			empty++
			text = "[no text on this page: a scan or an image]"
		}
		if err := out.write("## Page " + strconv.Itoa(n) + "\n" + strings.Trim(text, "\n") + "\n\n"); err != nil {
			return empty, err
		}
	}
	switch {
	case pages > maxPages:
		return empty, out.write("[" + strconv.Itoa(pages-maxPages) + " more pages not read]\n")
	case pages > 0 && empty == pages:
		return empty, out.write("[no page has text: the PDF is scanned]\n")
	}
	return empty, nil
}

// pageText is one page's text; a page the library cannot read is an
// error, not the end of the document.
func pageText(rd *pdf.Reader, n int) (text string, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("page %d: %s", n, short(v))
		}
	}()
	p := rd.Page(n)
	if p.V.IsNull() {
		return "", nil
	}
	return p.GetPlainText(nil)
}

// short is a recovered value as a short line, never the file's content
// at length.
func short(v any) string {
	s := fmt.Sprint(v)
	if len(s) > 120 {
		s = s[:validCut(s, 120)] + "…"
	}
	return s
}
