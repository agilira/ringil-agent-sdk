// emit.go: rendering a compiled node as JSON Schema
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package schema

import "encoding/json"

var kindNames = map[Kind]string{
	Object: "object", Map: "object", Array: "array", String: "string",
	Integer: "integer", Number: "number", Boolean: "boolean",
}

// JSON renders n as a JSON Schema document. Objects are closed
// (additionalProperties false) because validation refuses unknown fields:
// telling the model otherwise would invite calls that fail.
func (n *Node) JSON() (json.RawMessage, error) {
	return json.Marshal(n.doc())
}

func (n *Node) doc() map[string]any {
	d := map[string]any{}
	// Optional pointer fields are announced as optional, not as a
	// ["type","null"] union: several providers reject type unions, and
	// validation accepts null for them anyway.
	if name, ok := kindNames[n.Kind]; ok {
		d["type"] = name
	}
	setIf(d, "description", n.Description, n.Description != "")
	setIf(d, "format", n.Format, n.Format != "")
	setIf(d, "enum", n.Enum, len(n.Enum) > 0)
	n.bounds(d)
	switch n.Kind {
	case Object:
		n.objectDoc(d)
	case Map:
		d["additionalProperties"] = n.Elem.doc()
	case Array:
		d["items"] = n.Elem.doc()
	}
	return d
}

func setIf(d map[string]any, key string, value any, ok bool) {
	if ok {
		d[key] = value
	}
}

func (n *Node) bounds(d map[string]any) {
	minKey, maxKey := "minimum", "maximum"
	switch n.Kind {
	case String:
		minKey, maxKey = "minLength", "maxLength"
	case Array:
		minKey, maxKey = "minItems", "maxItems"
	}
	if n.Min != nil {
		d[minKey] = json.Number(n.Min.text)
	}
	if n.Max != nil {
		d[maxKey] = json.Number(n.Max.text)
	}
}

func (n *Node) objectDoc(d map[string]any) {
	props := map[string]any{}
	required := []string{}
	for _, f := range n.Fields {
		props[f.Name] = f.Node.doc()
		if f.Required {
			required = append(required, f.Name)
		}
	}
	d["properties"] = props
	d["additionalProperties"] = false
	setIf(d, "required", required, len(required) > 0)
}
