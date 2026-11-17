// tags.go: struct tags that refine a schema
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package schema

import (
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"
)

// Bound is a min or max limit. Integer bounds are kept exact: a float64
// cannot tell 2^53 from 2^53+1, and an int64 field must not accept a value
// past its max because of that.
type Bound struct {
	text string
	f    float64
	i    *big.Int // set for Integer nodes
}

// applyTags reads desc, enum, min and max. min and max bound a number's
// value, a string's length and an array's items.
func applyTags(n *Node, tag reflect.StructTag) error {
	n.Description = tag.Get("desc")
	if err := applyEnum(n, tag); err != nil {
		return err
	}
	var err error
	if n.Min, err = bound(n, tag, "min"); err != nil {
		return err
	}
	if n.Max, err = bound(n, tag, "max"); err != nil {
		return err
	}
	if n.Min != nil && n.Max != nil && compare(n.Min, n.Max) > 0 {
		return errors.New(ErrType, "min is greater than max")
	}
	return nil
}

// compare orders two bounds of the same node, which share a kind.
func compare(a, b *Bound) int {
	if a.i != nil {
		return a.i.Cmp(b.i)
	}
	switch {
	case a.f < b.f:
		return -1
	case a.f > b.f:
		return 1
	}
	return 0
}

func applyEnum(n *Node, tag reflect.StructTag) error {
	raw, ok := tag.Lookup("enum")
	if !ok {
		return nil
	}
	if n.Kind != String {
		return errors.New(ErrType, "enum applies to strings only")
	}
	for value := range strings.SplitSeq(raw, ",") {
		if value == "" {
			return errors.New(ErrType, "enum has an empty value")
		}
		n.Enum = append(n.Enum, value)
	}
	return nil
}

func bound(n *Node, tag reflect.StructTag, key string) (*Bound, error) {
	raw, ok := tag.Lookup(key)
	if !ok {
		return nil, nil
	}
	switch n.Kind {
	case Integer:
		return integerBound(raw, key)
	case Number:
		return numberBound(raw, key)
	case String, Array:
		return lengthBound(raw, key)
	}
	return nil, errors.New(ErrType, key+" applies to numbers, strings and arrays")
}

func integerBound(raw, key string) (*Bound, error) {
	i, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, errors.New(ErrType, key+" must be an integer")
	}
	return &Bound{text: i.String(), i: i}, nil
}

func numberBound(raw, key string) (*Bound, error) {
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, errors.New(ErrType, key+" is not a finite number")
	}
	return &Bound{text: strconv.FormatFloat(f, 'f', -1, 64), f: f}, nil
}

func lengthBound(raw, key string) (*Bound, error) {
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return nil, errors.New(ErrType, key+" must be a non-negative integer for strings and arrays")
	}
	return &Bound{text: strconv.Itoa(v), f: float64(v)}, nil
}
