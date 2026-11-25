// ooxml.go: Word, Excel and PowerPoint
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package document

import (
	"encoding/xml"
	"strconv"
	"strings"
)

// readWord: the body's paragraphs in order; a table's cells separated by
// " | ", a row a line. Deleted text (tracked changes) is left out.
func readWord(p *pack, out *writer) error {
	b, err := p.must("word/document.xml", Word)
	if err != nil {
		return err
	}
	r := &wordReader{out: out}
	return walk{start: r.start, end: r.end, text: r.text}.run(b, "word/document.xml")
}

type wordReader struct {
	out    *writer
	inText bool
	cells  int // depth of table cells
}

func (r *wordReader) start(name string, _ []xml.Attr) error {
	switch name {
	case "t":
		r.inText = true
	case "tab":
		return r.out.write("\t")
	case "br", "cr":
		return r.out.write(sep(r.cells, "\n"))
	case "tc":
		r.cells++
	}
	return nil
}

func (r *wordReader) end(name string) error {
	switch name {
	case "t":
		r.inText = false
	case "p":
		return r.out.write(sep(r.cells, "\n"))
	case "tc":
		r.cells--
		return r.out.write(" | ")
	case "tr":
		return r.out.write("\n")
	}
	return nil
}

func (r *wordReader) text(s string) error {
	if r.inText {
		return r.out.write(s)
	}
	return nil
}

// sep is what ends a paragraph or a line break: a new line, or a space
// inside a table cell (the cell's text stays on its row).
func sep(cells int, nl string) string {
	if cells > 0 {
		return " "
	}
	return nl
}

// readExcel: each sheet in the workbook's order, its rows with the cells'
// columns ("row 3: A=Total | C=42"). Values as stored: a date is the
// number Excel keeps, a formula its last result.
func readExcel(p *pack, out *writer) error {
	sheets, err := workbookSheets(p)
	if err != nil {
		return err
	}
	shared, err := sharedStrings(p)
	if err != nil {
		return err
	}
	for _, sh := range sheets {
		if err := writeSheet(p, sh, shared, out); err != nil {
			return err
		}
	}
	return nil
}

func writeSheet(p *pack, sh sheetRef, shared []string, out *writer) error {
	b, err := p.open(sh.part)
	if err != nil || b == nil {
		return err
	}
	if err := out.write("## Sheet: " + sh.name + "\n"); err != nil {
		return err
	}
	if err := readSheet(b, sh.part, shared, out); err != nil {
		return err
	}
	return out.write("\n")
}

type sheetRef struct{ name, part string }

func workbookSheets(p *pack) ([]sheetRef, error) {
	const book = "xl/workbook.xml"
	b, err := p.must(book, Excel)
	if err != nil {
		return nil, err
	}
	targets, err := rels(p, book)
	if err != nil {
		return nil, err
	}
	var out []sheetRef
	err = walk{start: func(name string, a []xml.Attr) error {
		if name == "sheet" {
			out = append(out, sheetRef{attr(a, "name"), targets[attrNS(a, relNS, "id")]})
		}
		return nil
	}}.run(b, book)
	return out, err
}

// sharedStrings is the workbook's table of strings: the cells of type "s"
// hold an index into it. Phonetic guides (rPh) are left out.
func sharedStrings(p *pack) ([]string, error) {
	b, err := p.open("xl/sharedStrings.xml")
	if err != nil || b == nil {
		return nil, err
	}
	r := &sstReader{}
	err = walk{start: r.start, end: r.end, text: r.text}.run(b, "xl/sharedStrings.xml")
	return r.out, err
}

type sstReader struct {
	out      []string
	cur      strings.Builder
	inText   bool
	phonetic int
}

func (r *sstReader) start(name string, _ []xml.Attr) error {
	switch name {
	case "si":
		r.cur.Reset()
	case "t":
		r.inText = true
	case "rPh":
		r.phonetic++
	}
	return nil
}

func (r *sstReader) end(name string) error {
	switch name {
	case "si":
		r.out = append(r.out, r.cur.String())
	case "t":
		r.inText = false
	case "rPh":
		r.phonetic--
	}
	return nil
}

func (r *sstReader) text(s string) error {
	if r.inText && r.phonetic == 0 {
		writeCell(&r.cur, s)
	}
	return nil
}

func readSheet(b []byte, part string, shared []string, out *writer) error {
	var row rowText
	var cell struct {
		ref, kind string
		value     strings.Builder
		inValue   bool
	}
	return walk{
		start: func(name string, a []xml.Attr) error {
			switch name {
			case "row":
				row.reset(attr(a, "r"))
			case "c":
				cell.ref, cell.kind = attr(a, "r"), attr(a, "t")
				cell.value.Reset()
			case "v", "t":
				cell.inValue = true
			}
			return nil
		},
		end: func(name string) error {
			switch name {
			case "v", "t":
				cell.inValue = false
			case "c":
				row.add(column(cell.ref, row.next), cellValue(cell.kind, cell.value.String(), shared))
			case "row":
				return row.flush(out)
			}
			return nil
		},
		text: func(s string) error {
			if cell.inValue {
				writeCell(&cell.value, s)
			}
			return nil
		},
	}.run(b, part)
}

func cellValue(kind, v string, shared []string) string {
	switch kind {
	case "s":
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && i >= 0 && i < len(shared) {
			return shared[i]
		}
		return ""
	case "b":
		if v == "1" {
			return "TRUE"
		}
		return "FALSE"
	}
	return v
}

// column is the column of a cell reference ("C12" is 2), or next when the
// reference is missing.
func column(ref string, next int) int {
	col := 0
	for i := 0; i < len(ref) && i < 4; i++ {
		c := ref[i] | 0x20
		if c < 'a' || c > 'z' {
			break
		}
		col = col*26 + int(c-'a'+1)
	}
	if col == 0 {
		return next
	}
	return col - 1
}

// readPowerPoint: the slides in the presentation's order, each with its
// paragraphs.
func readPowerPoint(p *pack, out *writer) error {
	const pres = "ppt/presentation.xml"
	b, err := p.must(pres, PowerPoint)
	if err != nil {
		return err
	}
	targets, err := rels(p, pres)
	if err != nil {
		return err
	}
	var slides []string
	err = walk{start: func(name string, a []xml.Attr) error {
		if name == "sldId" {
			slides = append(slides, targets[attrNS(a, relNS, "id")])
		}
		return nil
	}}.run(b, pres)
	if err != nil {
		return err
	}
	slide := 0
	for _, part := range slides {
		if err := readSlide(p, part, &slide, out); err != nil {
			return err
		}
	}
	return nil
}

func readSlide(p *pack, part string, slide *int, out *writer) error {
	b, err := p.open(part)
	if err != nil || b == nil {
		return err
	}
	*slide++
	if err := out.write("## Slide " + strconv.Itoa(*slide) + "\n"); err != nil {
		return err
	}
	return walkSlide(b, part, out)
}

func walkSlide(b []byte, part string, out *writer) error {
	inText := false
	err := walk{
		start: func(name string, _ []xml.Attr) error {
			switch name {
			case "t":
				inText = true
			case "br":
				return out.write("\n")
			}
			return nil
		},
		end: func(name string) error {
			switch name {
			case "t":
				inText = false
			case "p":
				return out.write("\n")
			}
			return nil
		},
		text: func(s string) error {
			if inText {
				return out.write(s)
			}
			return nil
		},
	}.run(b, part)
	if err != nil {
		return err
	}
	return out.write("\n")
}
