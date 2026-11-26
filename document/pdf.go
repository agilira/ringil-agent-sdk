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
// Limits.Decoded bounds what the streams decode to: a few kilobytes can
// decode to gigabytes, and one stream can be every page's content.
const maxPages = 2000

// pdfDoc is Read for a PDF.
func pdfDoc(r io.ReaderAt, size int64, limits Limits) (Doc, error) {
	limits = limits.withDefaults()
	out := &writer{max: limits.Text}
	scans, pages, err := readPDF(r, size, limits.Decoded, out)
	if err != nil && !errors.HasCode(err, errFull) {
		return Doc{}, err
	}
	return Doc{Kind: PDF, Text: out.String(), Cut: out.cut, Scans: scans, Pages: pages}, nil
}

// readPDF: the text of each page, in order, under "## Page N". A scanned
// page has no text: it is named as such, for the host to send the page
// to a model that reads images. The library reports a damaged file by
// panicking: that is turned into an error here (it starts no goroutine).
func readPDF(r io.ReaderAt, size, decoded int64, out *writer) (scans, pages int, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = errors.New(ErrFormat, "a damaged PDF ("+short(v)+")")
			scans, pages = 0, 0
		}
	}()
	rd, err := openPDF(r, size)
	if err != nil {
		return 0, 0, err
	}
	rd.SetDecodeLimit(decoded)
	scans, err = writePages(rd, decoded, out)
	return scans, rd.NumPage(), err
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
func writePages(rd *pdf.Reader, decoded int64, out *writer) (int, error) {
	pages, empty := rd.NumPage(), 0
	for n := 1; n <= pages && n <= maxPages; n++ {
		text, err := pageText(rd, n)
		if overDecoded(err) {
			return empty, out.write("[the PDF decodes to more than " + strconv.FormatInt(decoded>>20, 10) + " MiB: pages " + strconv.Itoa(n) + " to " + strconv.Itoa(pages) + " are not read]\n")
		}
		text, blank := pageNote(text, err)
		if blank {
			empty++
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

// overDecoded: the page stopped at the decode limit (the library reports
// it by its text: its panic is recovered as a new error).
func overDecoded(err error) bool {
	return err != nil && strings.Contains(err.Error(), pdf.ErrDecodeLimit.Error())
}

// pageNote is a page's text as written: a note for a page that could not
// be read, or has no text (blank).
func pageNote(text string, err error) (string, bool) {
	switch {
	case err != nil:
		return "[this page could not be read]", false
	case strings.TrimSpace(text) == "":
		return "[no text on this page: a scan or an image]", true
	}
	return text, false
}
