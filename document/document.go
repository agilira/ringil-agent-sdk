// document.go: the text of office documents
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package document reads the text of office documents: Word (.docx),
// Excel (.xlsx), PowerPoint (.pptx) and OpenDocument (.odt, .ods, .odp)
// with the standard library alone, and PDF (github.com/agilira/pdf, a fork of ledongthuc/pdf with reference cycles fixed,
// page by page). The office formats are zip files of XML; the text
// comes out in reading order, with the structure a reader needs: the
// sheets and their rows with cell references, the slides in their order,
// table cells separated by " | ".
//
// A document is data from anywhere: every read is bounded (the parts of
// the zip, their size once unpacked, the text, the cells a row may claim
// to repeat), so a zip bomb or a sheet of a million empty columns costs
// what its limits allow and no more. XML entities are never expanded
// beyond the five the language defines.
package document

import (
	"archive/zip"
	"bytes"
	"io"
	"path"
	"strings"
	"unicode"

	"github.com/agilira/go-errors"
)

// Error codes.
const (
	// ErrFormat: not a document this package reads, or a damaged one.
	ErrFormat errors.ErrorCode = "DOCUMENT_FORMAT"
	// ErrTooLarge: the document is past a limit.
	ErrTooLarge errors.ErrorCode = "DOCUMENT_TOO_LARGE"
)

// Limits bound one read. Zero fields take the defaults.
type Limits struct {
	// Part is the most bytes one part of the zip may unpack to; default
	// 64 MiB.
	Part int64
	// Total is the most bytes all the parts read may unpack to; default
	// 256 MiB.
	Total int64
	// Text is the most text returned; default 16 MiB. Past it the text
	// ends with a note.
	Text int
}

func (l Limits) withDefaults() Limits {
	if l.Part <= 0 {
		l.Part = 64 << 20
	}
	if l.Total <= 0 {
		l.Total = 256 << 20
	}
	if l.Text <= 0 {
		l.Text = 16 << 20
	}
	return l
}

// Kinds of document.
const (
	Word         = "Word document"
	Excel        = "Excel workbook"
	PowerPoint   = "PowerPoint presentation"
	Text         = "OpenDocument text"
	Spreadsheet  = "OpenDocument spreadsheet"
	Presentation = "OpenDocument presentation"
	PDF          = "PDF document"
)

// Doc is a document's text.
type Doc struct {
	Kind string
	Text string
	// Cut: the text stopped at Limits.Text.
	Cut bool
	// Scans counts a PDF's pages without text: scans or images, which a
	// model that reads PDFs can see.
	Scans int
}

// Kind returns the kind of document name is by its extension, or "".
func Kind(name string) string {
	return map[string]string{
		".docx": Word, ".docm": Word, ".xlsx": Excel, ".xlsm": Excel, ".pptx": PowerPoint, ".pptm": PowerPoint,
		".odt": Text, ".ods": Spreadsheet, ".odp": Presentation, ".pdf": PDF,
	}[strings.ToLower(path.Ext(name))]
}

// Read returns the text of the document in r, of size bytes, named name
// (its extension says the kind; a zip that is not that kind is refused).
func Read(r io.ReaderAt, size int64, name string, limits Limits) (Doc, error) {
	kind := Kind(name)
	if kind == "" {
		return Doc{}, unsupported(name)
	}
	if kind == PDF {
		return pdfDoc(r, size, limits)
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		if ole(r) {
			return Doc{}, errors.New(ErrFormat, "a password-protected or old binary Office file: open it, remove the password or save it as "+path.Ext(name)+", and read it again")
		}
		return Doc{}, errors.New(ErrFormat, "not a valid "+kind+": it is not a zip file")
	}
	p, err := newPack(zr, limits.withDefaults())
	if err != nil {
		return Doc{}, err
	}
	out := &writer{max: p.limits.Text}
	if err := readKind(p, out, kind); err != nil && !errors.HasCode(err, errFull) {
		return Doc{}, err
	}
	return Doc{Kind: kind, Text: out.String(), Cut: out.cut}, nil
}

func readKind(p *pack, out *writer, kind string) error {
	switch kind {
	case Word:
		return readWord(p, out)
	case Excel:
		return readExcel(p, out)
	case PowerPoint:
		return readPowerPoint(p, out)
	default:
		return readOpenDocument(p, out, kind)
	}
}

// ole: r starts as an OLE container, what Office writes for encrypted
// files and its old binary formats.
func ole(r io.ReaderAt) bool {
	head := make([]byte, 8)
	n, _ := r.ReadAt(head, 0)
	return n == 8 && bytes.Equal(head, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})
}

// unsupported names what to do with a format this package does not read.
func unsupported(name string) error {
	switch strings.ToLower(path.Ext(name)) {
	case ".doc", ".xls", ".ppt":
		return errors.New(ErrFormat, "an old binary Office file ("+path.Ext(name)+"): save it as "+path.Ext(name)+"x, or as PDF")
	}
	return errors.New(ErrFormat, "not an office document (.docx, .xlsx, .pptx, .odt, .ods, .odp, .pdf)")
}

// maxZipFiles bounds the directory entries indexed from one zip archive.
const maxZipFiles = 10000

// pack is the zip, read within the limits.
type pack struct {
	files  map[string]*zip.File
	limits Limits
	read   int64
}

func newPack(zr *zip.Reader, limits Limits) (*pack, error) {
	if len(zr.File) > maxZipFiles {
		return nil, errors.New(ErrTooLarge, "too many parts in the zip")
	}
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		k := strings.ToLower(f.Name)
		if _, dup := files[k]; !dup {
			files[k] = f
		}
	}
	return &pack{files: files, limits: limits}, nil
}

// open returns the unpacked bytes of the part at name, or nil when there
// is none.
func (p *pack) open(name string) ([]byte, error) {
	f := p.find(name)
	if f == nil {
		return nil, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, errors.New(ErrFormat, "a damaged part: "+name)
	}
	defer func() { _ = rc.Close() }()
	room := min(p.limits.Part, p.limits.Total-p.read)
	b, err := io.ReadAll(io.LimitReader(rc, room+1))
	if err != nil {
		return nil, errors.New(ErrFormat, "a damaged part: "+name)
	}
	if int64(len(b)) > room {
		return nil, errors.New(ErrTooLarge, "a part unpacks past the limit: "+name)
	}
	p.read += int64(len(b))
	return b, nil
}

// find is the part named name, case aside (writers differ), or nil.
func (p *pack) find(name string) *zip.File {
	return p.files[strings.ToLower(name)]
}

// must is open for a part the kind requires.
func (p *pack) must(name, kind string) ([]byte, error) {
	b, err := p.open(name)
	if err == nil && b == nil {
		err = errors.New(ErrFormat, "not a valid "+kind+": "+name+" is missing")
	}
	return b, err
}

// errFull stops a read when the text reached its limit; not an error.
const errFull errors.ErrorCode = "document_full"

// writer gathers the text up to max bytes.
type writer struct {
	b   bytes.Buffer
	max int
	cut bool
}

func (w *writer) write(s string) error {
	if w.cut {
		return errors.New(errFull, "full")
	}
	s = clean(s)
	if w.b.Len()+len(s) > w.max {
		w.b.WriteString(s[:validCut(s, w.max-w.b.Len())])
		w.b.WriteString("\n[the text goes on past the limit]\n")
		w.cut = true
		return errors.New(errFull, "full")
	}
	w.b.WriteString(s)
	return nil
}

func (w *writer) String() string {
	s := strings.TrimRight(w.b.String(), "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}

// validCut is n or less, not inside a UTF-8 character.
func validCut(s string, n int) int {
	n = max(min(n, len(s)), 0)
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return n
}

// clean drops control characters but tab and newline: they would reach a
// terminal and the model.
func clean(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, s)
}

func isControl(r rune) bool { return r != '\t' && r != '\n' && unicode.IsControl(r) }
