// validate.go: checking model arguments against a compiled schema
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package schema

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxProblems bounds the report: past a handful, more detail does not help
// the model correct its call.
const maxProblems = 8

// Problem is one reason the arguments were refused. Path and Message are
// built from the schema only: they never contain a value the model sent or
// the name of a field the schema does not define, so hostile input cannot
// steer what is reflected back.
type Problem struct {
	Path    string
	Message string
}

func (p Problem) String() string { return p.Path + ": " + p.Message }

// Validate checks data against n, which must be an object node, and
// returns the input re-encoded from what was validated. Callers must decode
// that canonical form, never the original bytes: encoding/json merges
// duplicate keys into structs, so the original could carry a value the
// validator never saw. A top-level null or empty input counts as an empty
// object, as models send both for tools without arguments.
func (n *Node) Validate(data []byte) ([]byte, []Problem) {
	value, ok := parse(data)
	if !ok {
		return nil, []Problem{{Path: "arguments", Message: "must be one JSON object"}}
	}
	if value == nil {
		value = map[string]any{}
	}
	v := &validator{}
	v.check(n, value, "arguments")
	if problems := v.result(); len(problems) > 0 {
		return nil, problems
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, []Problem{{Path: "arguments", Message: "could not be re-encoded"}}
	}
	return canonical, nil
}

func parse(data []byte) (any, bool) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, true
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return value, true
}

type validator struct {
	problems []Problem
	dropped  int
}

func (v *validator) add(path, msg string) {
	if len(v.problems) < maxProblems {
		v.problems = append(v.problems, Problem{path, msg})
		return
	}
	v.dropped++
}

func (v *validator) result() []Problem {
	if v.dropped > 0 {
		v.problems = append(v.problems, Problem{"arguments", strconv.Itoa(v.dropped) + " more problems not listed"})
	}
	return v.problems
}

func (v *validator) check(n *Node, value any, path string) {
	if value == nil {
		if !n.Nullable {
			v.add(path, "must not be null")
		}
		return
	}
	v.typed(n, value, path)
}

func (v *validator) typed(n *Node, value any, path string) {
	switch n.Kind {
	case Object:
		v.object(n, value, path)
	case Map:
		v.mapValue(n, value, path)
	case Array:
		v.array(n, value, path)
	case String:
		v.str(n, value, path)
	case Integer, Number:
		v.number(n, value, path)
	case Boolean:
		if _, ok := value.(bool); !ok {
			v.add(path, "must be a boolean")
		}
	}
}

func (v *validator) object(n *Node, value any, path string) {
	obj, ok := value.(map[string]any)
	if !ok {
		v.add(path, "must be an object")
		return
	}
	known := make(map[string]bool, len(n.Fields))
	for _, f := range n.Fields {
		known[f.Name] = true
		child, present := obj[f.Name]
		switch {
		case present:
			v.check(f.Node, child, join(path, f.Name))
		case f.Required:
			v.add(join(path, f.Name), "is required")
		}
	}
	for key := range obj {
		if !known[key] {
			v.add(path, "has fields the schema does not define; allowed: "+fieldNames(n))
			return
		}
	}
}

func join(path, name string) string {
	if path == "arguments" {
		return name
	}
	return path + "." + name
}

func fieldNames(n *Node) string {
	if len(n.Fields) == 0 {
		return "none"
	}
	names := make([]string, len(n.Fields))
	for i, f := range n.Fields {
		names[i] = f.Name
	}
	return strings.Join(names, ", ")
}

func (v *validator) mapValue(n *Node, value any, path string) {
	obj, ok := value.(map[string]any)
	if !ok {
		v.add(path, "must be an object")
		return
	}
	for _, child := range obj {
		// The key came from the model: the path names the map, not the key.
		v.check(n.Elem, child, path+".*")
	}
}

func (v *validator) array(n *Node, value any, path string) {
	items, ok := value.([]any)
	if !ok {
		v.add(path, "must be an array")
		return
	}
	if !within(n, float64(len(items))) {
		v.add(path, "must have "+rangeText(n)+" items")
	}
	for i, item := range items {
		v.check(n.Elem, item, path+"["+strconv.Itoa(i)+"]")
	}
}

func (v *validator) str(n *Node, value any, path string) {
	s, ok := value.(string)
	if !ok {
		v.add(path, "must be a string")
		return
	}
	switch {
	case len(n.Enum) > 0 && !slices.Contains(n.Enum, s):
		v.add(path, "must be one of: "+strings.Join(n.Enum, ", "))
	case !within(n, float64(utf8.RuneCountInString(s))):
		v.add(path, "must be "+rangeText(n)+" characters long")
	case n.Format == "date-time" && !isDateTime(s):
		v.add(path, "must be an RFC 3339 date-time, e.g. 2026-11-18T09:30:00Z")
	}
}

// isDateTime also requires the value to encode again: time.Parse accepts
// offsets such as +24:00 that time.Time cannot marshal, which would make the
// tool's own output fail later.
func isDateTime(s string) bool {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return false
	}
	_, err = t.MarshalJSON()
	return err == nil
}

func (v *validator) number(n *Node, value any, path string) {
	num, ok := value.(json.Number)
	if !ok {
		v.add(path, "must be a number")
		return
	}
	if n.Kind == Integer {
		v.integer(n, string(num), path)
		return
	}
	f, ok := parseFloat(string(num), n.bits)
	switch {
	case !ok:
		v.add(path, "must be a finite number")
	case !within(n, f):
		v.add(path, "must be "+rangeText(n))
	}
}

// parseFloat checks that s fits the Go float type and returns it as
// float64, so bounds are compared on the value the model wrote rather than
// on its float32 rounding.
func parseFloat(s string, bits int) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) {
		return 0, false
	}
	if bits == 32 {
		if _, err := strconv.ParseFloat(s, 32); err != nil {
			return 0, false
		}
	}
	return f, true
}

// integer checks s exactly: it must be an integer literal that fits the Go
// type, compared to its bounds without going through float64.
func (v *validator) integer(n *Node, s, path string) {
	i, ok := new(big.Int).SetString(s, 10)
	if !ok || !fits(i, n.bits, n.unsigned) {
		msg := "must be an integer that fits " + strconv.Itoa(n.bits) + " bits"
		if n.unsigned {
			msg = "must be a non-negative integer that fits " + strconv.Itoa(n.bits) + " bits"
		}
		v.add(path, msg)
		return
	}
	if (n.Min != nil && i.Cmp(n.Min.i) < 0) || (n.Max != nil && i.Cmp(n.Max.i) > 0) {
		v.add(path, "must be "+rangeText(n))
	}
}

func fits(i *big.Int, bits int, unsigned bool) bool {
	if unsigned {
		return i.Sign() >= 0 && i.BitLen() <= bits
	}
	lo := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), uint(bits-1)))
	hi := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bits-1)), big.NewInt(1))
	return i.Cmp(lo) >= 0 && i.Cmp(hi) <= 0
}

func within(n *Node, f float64) bool {
	return (n.Min == nil || f >= n.Min.f) && (n.Max == nil || f <= n.Max.f)
}

func rangeText(n *Node) string {
	switch {
	case n.Min != nil && n.Max != nil:
		return "between " + n.Min.text + " and " + n.Max.text
	case n.Min != nil:
		return "at least " + n.Min.text
	case n.Max != nil:
		return "at most " + n.Max.text
	}
	return "valid"
}
