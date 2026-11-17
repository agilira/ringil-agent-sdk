// schema.go: JSON schemas derived from Go types
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package schema derives a JSON schema from a Go struct type and validates
// model-produced arguments against it before they reach a tool.
//
// The same compiled [Node] produces the schema sent to the model and drives
// validation, so what the model is told and what is enforced cannot drift.
package schema

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/agilira/go-errors"
)

// ErrType is returned when a Go type cannot be described as a tool input.
const ErrType errors.ErrorCode = "SCHEMA_TYPE"

// Kind is the JSON type of a node.
type Kind int

// Kinds.
const (
	Object Kind = iota
	Array
	Map
	String
	Integer
	Number
	Boolean
	Any
)

// Node is a compiled schema for one value.
type Node struct {
	Kind        Kind
	Description string
	Nullable    bool
	Format      string
	Enum        []string
	// Min and Max bound the value of numbers, the length of strings and the
	// items of arrays.
	Min, Max *Bound
	Fields   []Field // Object
	Elem     *Node   // Array, Map
	bits     int     // Integer: 8, 16, 32 or 64
	unsigned bool    // Integer
}

// Field is one property of an object.
type Field struct {
	Name     string
	Node     *Node
	Required bool
}

var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// For compiles t, which must be a struct, into a schema.
func For(t reflect.Type) (*Node, error) {
	if t == nil || t.Kind() != reflect.Struct || t == timeType {
		return nil, errors.New(ErrType, "tool input must be a struct")
	}
	c := &compiler{visiting: map[reflect.Type]bool{}}
	return c.node(t)
}

type compiler struct {
	visiting map[reflect.Type]bool
}

func (c *compiler) node(t reflect.Type) (*Node, error) {
	nullable := false
	if t.Kind() == reflect.Pointer {
		t, nullable = t.Elem(), true
		if t.Kind() == reflect.Pointer {
			return nil, typeError(t, "pointer to pointer")
		}
	}
	n, err := c.base(t)
	if err != nil {
		return nil, err
	}
	n.Nullable = nullable
	return n, nil
}

func (c *compiler) base(t reflect.Type) (*Node, error) {
	switch t {
	case timeType:
		return &Node{Kind: String, Format: "date-time"}, nil
	case rawType:
		return &Node{Kind: Any}, nil
	}
	if customDecoding(t) {
		// Such a type decides for itself what JSON it accepts, so the schema
		// could not describe it and validation could not vouch for it. This
		// also catches structs that embed time.Time and inherit its decoder.
		return nil, typeError(t, "types with their own JSON or text decoding are not supported")
	}
	if n, ok := scalar(t); ok {
		return n, nil
	}
	switch t.Kind() {
	case reflect.Slice:
		return c.slice(t)
	case reflect.Map:
		return c.mapNode(t)
	case reflect.Struct:
		return c.object(t)
	}
	return nil, typeError(t, "unsupported kind")
}

var decoders = []reflect.Type{reflect.TypeFor[json.Unmarshaler](), reflect.TypeFor[encoding.TextUnmarshaler]()}

func customDecoding(t reflect.Type) bool {
	for _, iface := range decoders {
		if t.Implements(iface) || reflect.PointerTo(t).Implements(iface) {
			return true
		}
	}
	return false
}

func scalar(t reflect.Type) (*Node, bool) {
	switch t.Kind() {
	case reflect.String:
		return &Node{Kind: String}, true
	case reflect.Bool:
		return &Node{Kind: Boolean}, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return &Node{Kind: Integer, bits: t.Bits()}, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Node{Kind: Integer, bits: t.Bits(), unsigned: true}, true
	case reflect.Float32, reflect.Float64:
		return &Node{Kind: Number, bits: t.Bits()}, true
	}
	return nil, false
}

func (c *compiler) slice(t reflect.Type) (*Node, error) {
	if t.Elem().Kind() == reflect.Uint8 {
		// encoding/json turns []byte into base64, which models get wrong.
		return nil, typeError(t, "[]byte is not supported; use string")
	}
	elem, err := c.node(t.Elem())
	if err != nil {
		return nil, err
	}
	return &Node{Kind: Array, Elem: elem}, nil
}

func (c *compiler) mapNode(t reflect.Type) (*Node, error) {
	if t.Key().Kind() != reflect.String {
		return nil, typeError(t, "map keys must be strings")
	}
	elem, err := c.node(t.Elem())
	if err != nil {
		return nil, err
	}
	return &Node{Kind: Map, Elem: elem}, nil
}

func (c *compiler) object(t reflect.Type) (*Node, error) {
	if c.visiting[t] {
		return nil, typeError(t, "recursive type")
	}
	c.visiting[t] = true
	defer delete(c.visiting, t)
	n := &Node{Kind: Object}
	if err := c.fields(t, n, map[string]bool{}); err != nil {
		return nil, err
	}
	return n, nil
}

// fields appends the JSON-visible fields of t. Embedded structs without a
// JSON name are flattened, as encoding/json does.
// fields appends the JSON-visible fields of t. Embedded structs without a
// JSON name are flattened, as encoding/json does.
func (c *compiler) fields(t reflect.Type, n *Node, seen map[string]bool) error {
	for sf := range t.Fields() {
		name, opts, skip := jsonName(sf)
		if skip {
			continue
		}
		flatten, err := embedded(sf, name)
		if err != nil {
			return err
		}
		if flatten {
			err = c.fields(sf.Type, n, seen)
		} else {
			err = c.field(sf, name, opts, n, seen)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// embedded reports whether sf is an embedded struct to flatten.
func embedded(sf reflect.StructField, name string) (bool, error) {
	if !sf.Anonymous || name != "" {
		return false, nil
	}
	if sf.Type.Kind() == reflect.Pointer {
		// encoding/json flattens embedded pointers too, but leaves them nil
		// when absent: keep inputs simple and refuse them.
		return false, typeError(sf.Type, "embedded pointer fields are not supported")
	}
	return sf.Type.Kind() == reflect.Struct, nil
}

func (c *compiler) field(sf reflect.StructField, name, opts string, n *Node, seen map[string]bool) error {
	if err := checkTag(sf, name, opts); err != nil {
		return err
	}
	if name == "" {
		name = sf.Name
	}
	if seen[name] {
		return typeError(sf.Type, "duplicate JSON field "+name)
	}
	seen[name] = true
	node, err := c.node(sf.Type)
	if err != nil {
		return err
	}
	if err := applyTags(node, sf.Tag); err != nil {
		return errors.Wrap(err, ErrType, "field "+name)
	}
	optional := node.Nullable || hasOption(opts, "omitempty") || hasOption(opts, "omitzero")
	n.Fields = append(n.Fields, Field{Name: name, Node: node, Required: !optional})
	return nil
}

func checkTag(sf reflect.StructField, name, opts string) error {
	if name != "" && !validTagName(name) {
		// encoding/json would silently fall back to the Go field name, so
		// the schema and the decoder would disagree.
		return typeError(sf.Type, "invalid JSON field name "+strconv.Quote(name))
	}
	if hasOption(opts, "string") {
		return typeError(sf.Type, "the ,string option is not supported")
	}
	return nil
}

// jsonName returns the field's JSON name and options, and whether
// encoding/json ignores it.
func jsonName(sf reflect.StructField) (string, string, bool) {
	tag := sf.Tag.Get("json")
	if tag == "-" {
		return "", "", true
	}
	name, opts, _ := strings.Cut(tag, ",")
	if !sf.IsExported() && !(sf.Anonymous && name == "" && sf.Type.Kind() == reflect.Struct) {
		return "", "", true
	}
	return name, opts, false
}

// validTagName mirrors encoding/json's rule for tag names.
func validTagName(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", r) && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func hasOption(opts, want string) bool {
	for opt := range strings.SplitSeq(opts, ",") {
		if opt == want {
			return true
		}
	}
	return false
}

func typeError(t reflect.Type, msg string) error {
	return errors.New(ErrType, msg+": "+t.String())
}
