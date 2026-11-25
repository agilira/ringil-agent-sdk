// xml.go: walking the XML of a part
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package document

import (
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"strings"

	"github.com/agilira/go-errors"
)

// maxDepth bounds the nesting of the XML: past it the part is refused.
const maxDepth = 256

// walk calls start and end for the elements of b and text for its
// character data, by local name (the namespaces of the formats are fixed).
type walk struct {
	start func(name string, attr []xml.Attr) error
	end   func(name string) error
	text  func(s string) error
}

func (w walk) run(b []byte, part string) error {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	d.Entity = nil // the five predefined entities only
	depth := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New(ErrFormat, "the XML of "+part+" is damaged")
		}
		if err := w.token(tok, &depth, part); err != nil {
			return err
		}
	}
}

func (w walk) token(tok xml.Token, depth *int, part string) error {
	switch t := tok.(type) {
	case xml.StartElement:
		if *depth++; *depth > maxDepth {
			return errors.New(ErrTooLarge, "the XML of "+part+" nests too deep")
		}
		if w.start != nil {
			return w.start(t.Name.Local, t.Attr)
		}
	case xml.EndElement:
		*depth--
		if w.end != nil {
			return w.end(t.Name.Local)
		}
	case xml.CharData:
		if w.text != nil {
			return w.text(string(t))
		}
	}
	return nil
}

// attr is the value of the attribute with local name name, or "".
func attr(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// attrNS is the value of the attribute name in the namespace space.
func attrNS(attrs []xml.Attr, space, name string) string {
	for _, a := range attrs {
		if a.Name.Local == name && a.Name.Space == space {
			return a.Value
		}
	}
	return ""
}

const relNS = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"

// rels reads the relationships of a part (its _rels file): id → target,
// resolved against the part's folder.
func rels(p *pack, part string) (map[string]string, error) {
	dir, file := splitPart(part)
	b, err := p.open(dir + "_rels/" + file + ".rels")
	if err != nil || b == nil {
		return map[string]string{}, err
	}
	out := map[string]string{}
	err = walk{start: func(name string, a []xml.Attr) error {
		if name == "Relationship" && attr(a, "TargetMode") != "External" {
			out[attr(a, "Id")] = joinPart(dir, attr(a, "Target"))
		}
		return nil
	}}.run(b, part+" relationships")
	return out, err
}

func splitPart(part string) (dir, file string) {
	for i := len(part) - 1; i >= 0; i-- {
		if part[i] == '/' {
			return part[:i+1], part[i+1:]
		}
	}
	return "", part
}

// joinPart resolves target against dir as the formats do: "/x" from the
// root, "../x" up a folder. The result never leaves the zip's root.
func joinPart(dir, target string) string {
	if strings.HasPrefix(target, "/") {
		return path.Clean(target)[1:]
	}
	return path.Clean("/" + dir + target)[1:]
}
