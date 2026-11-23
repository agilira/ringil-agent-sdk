// portable_test.go: the structured-output form of a schema
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package schema

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type invoice struct {
	Number string  `json:"number" min:"3"`
	Total  float64 `json:"total" min:"0"`
	Lines  []struct {
		Item string `json:"item"`
	} `json:"lines"`
	Note *string `json:"note,omitempty"`
}

func TestPortableDropsBoundsAndClosesObjects(t *testing.T) {
	n, err := For(reflect.TypeFor[invoice]())
	if err != nil {
		t.Fatal(err)
	}
	if full, _ := n.JSON(); !strings.Contains(string(full), "minLength") {
		t.Fatalf("the test's bounds are not set: %s", full)
	}
	doc, all, err := n.Portable()
	if err != nil || all {
		t.Fatalf("%s %v %v", doc, all, err)
	}
	s := string(doc)
	if strings.Contains(s, "minimum") || strings.Contains(s, "minLength") || strings.Count(s, `"additionalProperties":false`) != 2 {
		t.Fatalf("%s", s)
	}
}

func TestPortableStrictAndMaps(t *testing.T) {
	type closed struct {
		A string `json:"a"`
	}
	n, _ := For(reflect.TypeFor[closed]())
	if _, all, _ := n.Portable(); !all {
		t.Fatal("every field required, not said")
	}
	n, _ = For(reflect.TypeFor[struct {
		M map[string]string `json:"m"`
	}]())
	if _, _, err := n.Portable(); !errors.Is(err, ErrNotPortable) {
		t.Fatalf("a map: %v", err)
	}
}

func TestPortableEmptyObjectHasRequired(t *testing.T) {
	type empty struct {
		Meta struct{} `json:"meta"`
	}
	n, err := For(reflect.TypeFor[empty]())
	if err != nil {
		t.Fatal(err)
	}
	doc, all, err := n.Portable()
	if err != nil || !all || strings.Count(string(doc), `"required":[]`) != 1 {
		t.Fatalf("%s all=%v err=%v", doc, all, err)
	}
	bare, _ := For(reflect.TypeFor[struct{}]())
	doc, all, err = bare.Portable()
	if err != nil || !all || !strings.Contains(string(doc), `"required":[]`) {
		t.Fatalf("%s all=%v err=%v", doc, all, err)
	}
}
