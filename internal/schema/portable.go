// portable.go: the schema every provider's structured output accepts
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package schema

import (
	"encoding/json"
	"errors"
)

// ErrNotPortable: a schema no provider's structured output takes: a map
// (they want closed objects) or raw JSON (a value of no type).
var ErrNotPortable = errors.New("a map or raw JSON has no structured-output form: use a struct or a list of structs")

// Portable renders n as the JSON Schema providers' structured output
// accepts: closed objects, no bounds (Validate checks those after), and
// whether every field is required, which strict modes want. A map has no
// such form.
func (n *Node) Portable() (json.RawMessage, bool, error) {
	all := true
	d, err := n.portable(&all)
	if err != nil {
		return nil, false, err
	}
	doc, err := json.Marshal(d)
	return doc, all, err
}

func (n *Node) portable(all *bool) (map[string]any, error) {
	d := map[string]any{}
	if name, ok := kindNames[n.Kind]; ok {
		d["type"] = name
	}
	setIf(d, "description", n.Description, n.Description != "")
	setIf(d, "format", n.Format, n.Format != "")
	setIf(d, "enum", n.Enum, len(n.Enum) > 0)
	switch n.Kind {
	case Map, Any:
		return nil, ErrNotPortable
	case Array:
		items, err := n.Elem.portable(all)
		if err != nil {
			return nil, err
		}
		d["items"] = items
	case Object:
		return d, n.portableFields(d, all)
	}
	return d, nil
}

func (n *Node) portableFields(d map[string]any, all *bool) error {
	props := map[string]any{}
	required := []string{}
	for _, f := range n.Fields {
		doc, err := f.Node.portable(all)
		if err != nil {
			return err
		}
		props[f.Name] = doc
		if f.Required {
			required = append(required, f.Name)
		} else {
			*all = false
		}
	}
	d["properties"], d["additionalProperties"], d["required"] = props, false, required
	return nil
}
