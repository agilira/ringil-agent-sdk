// document_test.go: tests for the text of office documents
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package document

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

// zipOf makes a zip of the parts, name → content.
func zipOf(t testing.TB, parts map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, body := range parts {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func read(t testing.TB, name string, parts map[string]string, l Limits) (Doc, error) {
	t.Helper()
	b := zipOf(t, parts)
	return Read(bytes.NewReader(b), int64(len(b)), name, l)
}

func mustRead(t *testing.T, name string, parts map[string]string) string {
	t.Helper()
	d, err := read(t, name, parts, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return d.Text
}

const w = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

func TestWord(t *testing.T) {
	got := mustRead(t, "a.docx", map[string]string{"word/document.xml": `<w:document ` + w + `><w:body>
		<w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:tab/><w:t xml:space="preserve"> world</w:t></w:r></w:p>
		<w:p><w:r><w:delText>gone</w:delText><w:t>kept</w:t><w:br/><w:t>next</w:t></w:r></w:p>
		<w:tbl><w:tr><w:tc><w:p><w:r><w:t>a</w:t></w:r></w:p><w:p><w:r><w:t>b</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>c</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
		<w:p><w:r><w:t>a &amp; b &lt;3</w:t></w:r></w:p>
	</w:body></w:document>`})
	want := "Hello\t world\nkept\nnext\na b  | c  | \na & b <3\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

const relsNS = `xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`

func TestExcel(t *testing.T) {
	got := mustRead(t, "b.xlsx", map[string]string{
		"xl/workbook.xml":            `<workbook ` + relsNS + `><sheets><sheet name="Second" r:id="rId2"/><sheet name="First" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Target="/xl/worksheets/sheet2.xml"/><Relationship Id="x" TargetMode="External" Target="https://evil"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst><si><t>Total</t></si><si><r><t>rich</t></r><r><t> text</t></r><rPh><t>ignored</t></rPh></si></sst>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData><row r="3"><c r="A3" t="s"><v>0</v></c><c r="C3"><v>42</v></c><c r="D3" t="b"><v>1</v></c></row><row r="4"><c r="B4"/></row></sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml":   `<worksheet><sheetData><row r="1"><c r="AA1" t="inlineStr"><is><t>inline</t></is></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>99</v></c></row></sheetData></worksheet>`,
	})
	want := "## Sheet: Second\nrow 1: B=rich text | AA=inline\n\n## Sheet: First\nrow 3: A=Total | C=42 | D=TRUE\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestPowerPoint(t *testing.T) {
	got := mustRead(t, "c.pptx", map[string]string{
		"ppt/presentation.xml":            `<p:presentation xmlns:p="p" ` + relsNS + `><p:sldIdLst><p:sldId r:id="rId3"/><p:sldId r:id="rId2"/></p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<Relationships><Relationship Id="rId2" Target="slides/slide1.xml"/><Relationship Id="rId3" Target="slides/slide2.xml"/></Relationships>`,
		"ppt/slides/slide1.xml":           `<p:sld xmlns:p="p" xmlns:a="a"><a:p><a:r><a:t>one</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide2.xml":           `<p:sld xmlns:p="p" xmlns:a="a"><a:p><a:r><a:t>two</a:t></a:r><a:br/><a:r><a:t>lines</a:t></a:r></a:p></p:sld>`,
	})
	want := "## Slide 1\ntwo\nlines\n\n## Slide 2\none\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

const odfNS = `xmlns:office="o" xmlns:text="t" xmlns:table="tb" xmlns:draw="d"`

func TestOpenDocumentText(t *testing.T) {
	got := mustRead(t, "d.odt", map[string]string{"content.xml": `<office:document-content ` + odfNS + `><office:body><office:text>
		<text:h>Title</text:h><text:p>a<text:s text:c="3"/>b<text:tab/>c<text:line-break/>d</text:p>
		<table:table><table:table-row><table:table-cell><text:p>x</text:p></table:table-cell><table:table-cell table:number-columns-repeated="1000000"/><table:table-cell><text:p>y</text:p></table:table-cell></table:table-row></table:table>
	</office:text></office:body></office:document-content>`})
	want := "Title\na   b\tc\nd\nx | y\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestOpenDocumentSpreadsheet(t *testing.T) {
	got := mustRead(t, "e.ods", map[string]string{"content.xml": `<office:document-content ` + odfNS + `><office:body><office:spreadsheet>
		<table:table table:name="Data">
		<table:table-row table:number-rows-repeated="1048570"><table:table-cell table:number-columns-repeated="16384"/></table:table-row>
		<table:table-row><table:table-cell office:value-type="float" office:value="3.5"><text:p>3,50 €</text:p></table:table-cell><table:table-cell table:number-columns-repeated="2"><text:p>z</text:p></table:table-cell></table:table-row>
		</table:table>
	</office:spreadsheet></office:body></office:document-content>`})
	want := "## Sheet: Data\nrow 1048571: A=3.5 | B=z | C=z\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestOpenDocumentPresentation(t *testing.T) {
	got := mustRead(t, "f.odp", map[string]string{"content.xml": `<office:document-content ` + odfNS + `><office:body><office:presentation>
		<draw:page><draw:frame><draw:text-box><text:p>first</text:p></draw:text-box></draw:frame></draw:page>
		<draw:page><draw:frame><draw:text-box><text:p>second</text:p></draw:text-box></draw:frame></draw:page>
	</office:presentation></office:body></office:document-content>`})
	if got != "## Slide 1\nfirst\n\n## Slide 2\nsecond\n" {
		t.Fatalf("got %q", got)
	}
}

func TestRefused(t *testing.T) {
	for name, want := range map[string]string{"x.doc": "old binary", "x.pdf": "PDF", "x.txt": "not an office", "x.docx": "not a zip"} {
		_, err := Read(bytes.NewReader([]byte("plain")), 5, name, Limits{})
		if !errors.HasCode(err, ErrFormat) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ole := append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, make([]byte, 100)...)
	if _, err := Read(bytes.NewReader(ole), int64(len(ole)), "x.docx", Limits{}); err == nil || !strings.Contains(err.Error(), "password") {
		t.Errorf("encrypted: %v", err)
	}
	if _, err := read(t, "x.xlsx", map[string]string{"other.xml": "<a/>"}, Limits{}); !errors.HasCode(err, ErrFormat) || !strings.Contains(err.Error(), "xl/workbook.xml") {
		t.Errorf("missing part: %v", err)
	}
	if _, err := read(t, "x.docx", map[string]string{"word/document.xml": "<a><b></a>"}, Limits{}); !errors.HasCode(err, ErrFormat) {
		t.Errorf("bad xml: %v", err)
	}
}

// A part that unpacks past the limit is refused; text past its limit is
// cut with a note, not an error.
func TestLimits(t *testing.T) {
	big := `<w:document ` + w + `><w:body><w:p><w:r><w:t>` + strings.Repeat("é", 4000) + `</w:t></w:r></w:p></w:body></w:document>`
	if _, err := read(t, "x.docx", map[string]string{"word/document.xml": big}, Limits{Part: 1000}); !errors.HasCode(err, ErrTooLarge) {
		t.Errorf("part: %v", err)
	}
	d, err := read(t, "x.docx", map[string]string{"word/document.xml": big}, Limits{Text: 101})
	if err != nil || !d.Cut || !strings.Contains(d.Text, "past the limit") || !strings.HasPrefix(d.Text, strings.Repeat("é", 50)+"\n") {
		t.Errorf("text: %v %q", err, d.Text[:min(len(d.Text), 120)])
	}
	deep := strings.Repeat("<a>", maxDepth+1) + strings.Repeat("</a>", maxDepth+1)
	if _, err := read(t, "x.docx", map[string]string{"word/document.xml": deep}, Limits{}); !errors.HasCode(err, ErrTooLarge) {
		t.Errorf("depth: %v", err)
	}
}

// A relationship never leaves the zip's root, and control characters
// never reach the text.
func TestPathsAndControls(t *testing.T) {
	if got := joinPart("xl/", "../../../etc/passwd"); got != "etc/passwd" {
		t.Errorf("join %q", got)
	}
	got := mustRead(t, "a.docx", map[string]string{"word/document.xml": `<w:document ` + w + `><w:body><w:p><w:r><w:t>a&#x9b;31mb&#x85;</w:t></w:r></w:p></w:body></w:document>`})
	if got != "a31mb\n" {
		t.Errorf("controls kept: %q", got)
	}
}

func TestKind(t *testing.T) {
	if Kind("A.DOCX") != Word || Kind("x.ods") != Spreadsheet || Kind("x.pdf") != PDF {
		t.Error("kinds")
	}
}

func TestEdgeCases(t *testing.T) {
	if got := mustRead(t, "empty.docx", map[string]string{"word/document.xml": `<w:document ` + w + `><w:body/></w:document>`}); got != "" {
		t.Errorf("empty docx = %q, want empty", got)
	}
	xlsxMissing := mustRead(t, "missing.xlsx", map[string]string{
		"xl/workbook.xml":            `<workbook ` + relsNS + `><sheets><sheet name="Ghost" r:id="rMissing"/><sheet name="Real" r:id="r1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="r1" Target="s1.xml"/></Relationships>`,
		"xl/s1.xml":                  `<worksheet><sheetData><row r="1"><c r="ZZZZ9"><v>drop</v></c><c><v>kept</v></c></row></sheetData></worksheet>`,
	})
	if xlsxMissing != "## Sheet: Real\nrow 1: A=kept\n" {
		t.Errorf("xlsx edge = %q", xlsxMissing)
	}
	pptxMissing := mustRead(t, "missing.pptx", map[string]string{
		"ppt/presentation.xml":            `<p:presentation xmlns:p="p" ` + relsNS + `><p:sldIdLst><p:sldId r:id="rMissing"/><p:sldId r:id="r1"/></p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<Relationships><Relationship Id="r1" Target="s1.xml"/></Relationships>`,
		"ppt/s1.xml":                      `<p:sld xmlns:p="p" xmlns:a="a"><a:p><a:r><a:t>ok</a:t></a:r></a:p></p:sld>`,
	})
	if pptxMissing != "## Slide 1\nok\n" {
		t.Errorf("pptx edge = %q", pptxMissing)
	}
}
