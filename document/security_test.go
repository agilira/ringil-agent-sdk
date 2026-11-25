// security_test.go: the document reader at its limits
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package document

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

// FuzzZip: any bytes, under any name, never panic and never give text
// past the limit or that is not UTF-8.
func FuzzZip(f *testing.F) {
	f.Add(zipOf(f, map[string]string{"word/document.xml": "<w:t>x</w:t>"}), "a.docx")
	f.Add([]byte("PK\x03\x04garbage"), "a.xlsx")
	f.Fuzz(func(t *testing.T, b []byte, name string) {
		d, err := Read(bytes.NewReader(b), int64(len(b)), name, Limits{Text: 4096})
		if err == nil {
			check(t, d, 4096)
		}
	})
}

// FuzzXML: any XML in the part each kind reads.
func FuzzXML(f *testing.F) {
	f.Add(`<w:t>a</w:t><w:p/>`, uint8(0))
	f.Add(`<table:table-row table:number-rows-repeated="99999"><table:table-cell table:number-columns-repeated="99999"><text:p>x</text:p></table:table-cell></table:table-row>`, uint8(4))
	f.Add(`<row r="-1"><c r="ZZZZZZ9" t="s"><v>-5</v></c></row>`, uint8(1))
	f.Fuzz(func(t *testing.T, x string, k uint8) {
		var parts map[string]string
		var name string
		switch k % 6 {
		case 0:
			name, parts = "a.docx", map[string]string{"word/document.xml": "<d>" + x + "</d>"}
		case 1:
			name = "a.xlsx"
			parts = map[string]string{
				"xl/workbook.xml":            `<workbook xmlns:r="` + relNS + `"><sheets><sheet name="s" r:id="r1"/></sheets></workbook>`,
				"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="r1" Target="s.xml"/></Relationships>`,
				"xl/s.xml":                   "<w>" + x + "</w>", "xl/sharedStrings.xml": "<sst>" + x + "</sst>",
			}
		case 2:
			name = "a.pptx"
			parts = map[string]string{
				"ppt/presentation.xml":            `<p xmlns:r="` + relNS + `"><sldId r:id="r1"/></p>`,
				"ppt/_rels/presentation.xml.rels": `<Relationships><Relationship Id="r1" Target="s.xml"/></Relationships>`,
				"ppt/s.xml":                       "<s>" + x + "</s>",
			}
		default:
			name = []string{"a.odt", "a.ods", "a.odp"}[k%6-3]
			parts = map[string]string{"content.xml": `<c xmlns:text="t" xmlns:table="tb" xmlns:office="o" xmlns:draw="d">` + x + "</c>"}
		}
		d, err := read(t, name, parts, Limits{Text: 1 << 16})
		if err == nil {
			check(t, d, 1<<16)
		}
	})
}

func check(t *testing.T, d Doc, limit int) {
	t.Helper()
	if !utf8.ValidString(d.Text) {
		t.Fatalf("not UTF-8: %q", d.Text)
	}
	if len(d.Text) > limit+64 {
		t.Fatalf("%d bytes past the limit %d", len(d.Text), limit)
	}
}

func bigXLSXParts(cells int) map[string]string {
	cellStr := "a" + strings.Repeat("é", maxCell)
	row := strings.Repeat(`<c t="s"><v>0</v></c>`, cells)
	return map[string]string{
		"xl/workbook.xml":            `<workbook ` + relsNS + `><sheets><sheet name="s" r:id="r1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="r1" Target="s.xml"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst><si><t>` + cellStr + `</t></si></sst>`,
		"xl/s.xml":                   `<worksheet><sheetData><row r="1">` + row + `</row></sheetData></worksheet>`,
	}
}

func TestCellAndRowBounds(t *testing.T) {
	d, err := read(t, "big.xlsx", bigXLSXParts(1), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(d.Text) || len(d.Text) > maxCell+64 {
		t.Fatalf("cell bound failed: len=%d valid=%v", len(d.Text), utf8.ValidString(d.Text))
	}
	d, err = read(t, "row.xlsx", bigXLSXParts(256), Limits{Text: 4096})
	if err != nil || !d.Cut {
		t.Fatalf("xlsx row bound: err=%v cut=%v", err, d.Cut)
	}
	check(t, d, 4096)

	odsVal := "a" + strings.Repeat("é", maxCell)
	odsParts := map[string]string{
		"content.xml": `<office:document-content ` + odfNS + `><office:body><office:spreadsheet><table:table table:name="s"><table:table-row>` +
			`<table:table-cell office:value-type="float" office:value="` + odsVal + `" table:number-columns-repeated="256"/>` +
			`</table:table-row></table:table></office:spreadsheet></office:body></office:document-content>`,
	}
	d, err = read(t, "row.ods", odsParts, Limits{Text: 4096})
	if err != nil || !d.Cut {
		t.Fatalf("ods row bound: err=%v cut=%v", err, d.Cut)
	}
	check(t, d, 4096)
}

func TestTooManyZipParts(t *testing.T) {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for i := range maxZipFiles + 1 {
		if _, err := zw.Create(fmt.Sprintf("p%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(bytes.NewReader(b.Bytes()), int64(b.Len()), "a.docx", Limits{}); !errors.HasCode(err, ErrTooLarge) {
		t.Fatalf("got %v, want %v", err, ErrTooLarge)
	}
}
