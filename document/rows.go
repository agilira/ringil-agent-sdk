// rows.go: a spreadsheet's row as a line
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package document

import (
	"sort"
	"strconv"
	"strings"
)

// maxColumns bounds the columns of a row: a reference past it is left
// out (Excel itself stops at 16384). maxCell bounds a single cell's text,
// maxRow a row's (the default text limit: a row past it could not be
// written whole anyway): 16384 cells all naming one long shared string
// are not held whole.
const (
	maxColumns = 16384
	maxCell    = 32 << 10
	maxRow     = 16 << 20
)

// writeCell appends s to b up to maxCell bytes without splitting a UTF-8 rune.
func writeCell(b *strings.Builder, s string) {
	rem := maxCell - b.Len()
	if rem <= 0 {
		return
	}
	if len(s) > rem {
		s = s[:validCut(s, rem)]
	}
	b.WriteString(s)
}

func writeParts(out *writer, parts ...string) error {
	for _, s := range parts {
		if s == "" {
			continue
		}
		if err := out.write(s); err != nil {
			return err
		}
	}
	return nil
}

// rowText gathers the non-empty cells of one row.
type rowText struct {
	num   int
	next  int // the column a cell without a reference takes
	cells map[int]string
	size  int // the bytes of cells
}

func (r *rowText) reset(ref string) {
	n, err := strconv.Atoi(ref)
	if err != nil || n <= 0 {
		n = r.num + 1
	}
	r.num, r.next, r.cells, r.size = n, 0, map[int]string{}, 0
}

// add sets column col; empty values are not kept.
func (r *rowText) add(col int, v string) {
	if col < 0 || col >= maxColumns {
		return
	}
	r.next = col + 1
	if v = strings.TrimSpace(v); v != "" {
		if len(v) > maxCell {
			v = v[:validCut(v, maxCell)]
		}
		v = strings.ReplaceAll(strings.ReplaceAll(v, "\r", ""), "\n", " ")
		size := r.size + len(v) - len(r.cells[col])
		if size > maxRow {
			return // the row is already longer than can be written
		}
		if r.cells == nil {
			r.cells = map[int]string{}
		}
		r.size, r.cells[col] = size, v
	}
}

// flush writes the row, when it has a value: "row 3: A=Total | C=42".
func (r *rowText) flush(out *writer) error {
	if len(r.cells) == 0 {
		return nil
	}
	cells := r.cells
	r.cells, r.size = nil, 0
	cols := make([]int, 0, len(cells))
	for c := range cells {
		cols = append(cols, c)
	}
	sort.Ints(cols)
	if err := writeParts(out, "row ", strconv.Itoa(r.num), ": "); err != nil {
		return err
	}
	for i, c := range cols {
		sep := ""
		if i > 0 {
			sep = " | "
		}
		if err := writeParts(out, sep, columnName(c), "=", cells[c]); err != nil {
			return err
		}
	}
	return out.write("\n")
}

// columnName is the letters of column c, from 0: A, B, ..., Z, AA.
func columnName(c int) string {
	name := ""
	for c++; c > 0; c = (c - 1) / 26 {
		name = string(rune('A'+(c-1)%26)) + name
	}
	return name
}
