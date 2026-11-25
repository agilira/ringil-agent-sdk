// odf.go: OpenDocument text, spreadsheets and presentations
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package document

import (
	"encoding/xml"
	"strconv"
	"strings"
)

// Bounds of the repetitions OpenDocument writes in a number: an empty
// cell or row repeated a million times costs nothing; one with a value is
// repeated at most this many times.
const (
	maxRepeatCells = 256
	maxRepeatRows  = 64
	maxSpaces      = 64
)

// readOpenDocument: the text of content.xml. Text: paragraphs and
// headings, tables a row a line. Spreadsheet: each sheet, its rows with
// the cells' columns. Presentation: each slide with its paragraphs.
func readOpenDocument(p *pack, out *writer, kind string) error {
	b, err := p.must("content.xml", kind)
	if err != nil {
		return err
	}
	o := &odf{out: out, sheets: kind == Spreadsheet}
	return walk{start: o.start, end: o.end, text: o.text}.run(b, "content.xml")
}

type odf struct {
	out    *writer
	sheets bool // a spreadsheet: rows with cell references

	slides   int
	cells    int // depth of table cells
	row      rowText
	col      int // the next cell's column
	cellText strings.Builder
	repeat   struct{ cells, rows int }
	inText   int // inside text:p or text:h
}

func (o *odf) start(name string, a []xml.Attr) error {
	switch name {
	case "page": // draw:page, a slide
		o.slides++
		return o.out.write("## Slide " + strconv.Itoa(o.slides) + "\n")
	case "table":
		o.row = rowText{}
		if o.sheets {
			return o.out.write("## Sheet: " + attr(a, "name") + "\n")
		}
	case "table-row":
		o.repeat.rows = repeats(attr(a, "number-rows-repeated"))
		o.row.reset("")
		o.col = 0
	case "table-cell", "covered-table-cell":
		o.startCell(a)
	case "p", "h":
		if o.inText >= 0 {
			o.inText++
		}
	}
	return o.inline(name, a)
}

func (o *odf) startCell(a []xml.Attr) {
	o.cells++
	o.repeat.cells = repeats(attr(a, "number-columns-repeated"))
	o.cellText.Reset()
	if v := attr(a, "value"); v != "" && attr(a, "value-type") != "string" {
		writeCell(&o.cellText, v) // a number or a date as stored, not as shown
		o.inText = -1             // its shown text is not added
	}
}

// inline is what the text's own elements write.
func (o *odf) inline(name string, a []xml.Attr) error {
	switch name {
	case "s":
		n := min(repeats(attr(a, "c")), maxSpaces)
		return o.write(strings.Repeat(" ", n))
	case "tab":
		return o.write("\t")
	case "line-break":
		return o.write(sep(o.cells, "\n"))
	}
	return nil
}

func (o *odf) end(name string) error {
	switch name {
	case "p", "h":
		if o.inText > 0 {
			o.inText--
		}
		if o.cells > 0 {
			return o.write(" ")
		}
		return o.out.write("\n")
	case "table-cell", "covered-table-cell":
		return o.endCell()
	case "table-row":
		return o.endRow()
	case "table", "page":
		return o.out.write("\n")
	}
	return nil
}

func (o *odf) endCell() error {
	o.cells--
	if o.inText < 0 {
		o.inText = 0
	}
	v := strings.TrimSpace(o.cellText.String())
	n := o.repeat.cells
	if v != "" {
		n = min(n, maxRepeatCells)
	}
	for range n {
		if v == "" {
			break // empty cells only move the column, however many
		}
		o.row.add(o.col, v)
		o.col++
	}
	switch {
	case v == "" && o.sheets:
		o.col = min(o.col+n, maxColumns) // the next cell keeps its column
	case v == "":
		o.col++ // in a text's table only the order counts
	}
	return nil
}

func (o *odf) endRow() error {
	if len(o.row.cells) == 0 {
		o.row.num += o.repeat.rows - 1 // empty rows only move the count
		return nil
	}
	cells := o.row.cells
	n := min(o.repeat.rows, maxRepeatRows)
	for i := range n {
		if i > 0 {
			o.row.num++
			o.row.cells = cells
		}
		if err := o.rowOut(); err != nil {
			return err
		}
	}
	return nil
}

// rowOut writes a row: with cell references in a spreadsheet, the cells
// separated by " | " in a text or a slide.
func (o *odf) rowOut() error {
	if o.sheets {
		return o.row.flush(o.out)
	}
	cells := o.row.cells
	o.row.cells = nil
	first := true
	for c := 0; c < o.col && c < maxColumns; c++ {
		v, ok := cells[c]
		if !ok {
			continue
		}
		sep := " | "
		if first {
			sep = ""
			first = false
		}
		if err := writeParts(o.out, sep, v); err != nil {
			return err
		}
	}
	return o.out.write("\n")
}

func (o *odf) text(s string) error {
	if o.inText <= 0 {
		return nil
	}
	return o.write(s)
}

// write puts text in the cell being read, or in the document.
func (o *odf) write(s string) error {
	if o.cells > 0 {
		if o.inText >= 0 {
			writeCell(&o.cellText, s)
		}
		return nil
	}
	return o.out.write(s)
}

// repeats reads a repetition count: missing or bad is 1.
func repeats(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 1
	}
	return min(n, 1<<20)
}
