// schema_test.go: tests for schema compilation and rendering
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"
)

type inner struct {
	Note string `json:"note,omitempty"`
}

type Embedded struct {
	Shared string `json:"shared"`
}

type full struct {
	Embedded
	City     string          `json:"city" desc:"City name" min:"1" max:"80"`
	Unit     string          `json:"unit,omitempty" enum:"c,f"`
	Days     int8            `json:"days" min:"1" max:"7"`
	Count    uint16          `json:"count,omitzero"`
	Ratio    float32         `json:"ratio,omitempty" min:"0" max:"1"`
	Exact    *bool           `json:"exact"`
	Tags     []string        `json:"tags,omitempty" max:"3"`
	At       time.Time       `json:"at,omitzero"`
	Extra    json.RawMessage `json:"extra,omitempty"`
	Labels   map[string]int  `json:"labels,omitempty"`
	Nested   inner           `json:"nested,omitzero"`
	Many     []inner         `json:"many,omitempty"`
	Untagged string          `json:",omitempty"`
	Skipped  string          `json:"-"`
	//lint:ignore U1000 proves unexported fields are skipped
	hidden string
	Opt    *map[string]inner `json:"opt"`
}

func compile(t *testing.T, v any) *Node {
	t.Helper()
	n, err := For(reflect.TypeOf(v))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func render(t *testing.T, n *Node) map[string]any {
	t.Helper()
	data, err := n.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestFieldsAndRequired(t *testing.T) {
	n := compile(t, full{})
	got := map[string]bool{}
	for _, f := range n.Fields {
		got[f.Name] = f.Required
	}
	want := map[string]bool{
		"shared": true, "city": true, "unit": false, "days": true, "count": false, "ratio": false,
		"exact": false, "tags": false, "at": false, "extra": false, "labels": false, "nested": false,
		"many": false, "Untagged": false, "opt": false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields %v\nwant   %v", got, want)
	}
}

func TestRenderedSchema(t *testing.T) {
	doc := render(t, compile(t, full{}))
	if doc["type"] != "object" || doc["additionalProperties"] != false {
		t.Fatalf("root %v", doc)
	}
	props := doc["properties"].(map[string]any)
	city := props["city"].(map[string]any)
	if city["description"] != "City name" || city["minLength"] != 1.0 || city["maxLength"] != 80.0 {
		t.Fatalf("city %v", city)
	}
	checkProp(t, props, "unit", "enum", []any{"c", "f"})
	checkProp(t, props, "days", "maximum", 7.0)
	checkProp(t, props, "tags", "maxItems", 3.0)
	checkProp(t, props, "at", "format", "date-time")
	checkProp(t, props, "exact", "type", "boolean")
	checkProp(t, props, "count", "type", "integer")
	checkProp(t, props, "ratio", "type", "number")
	if _, typed := props["extra"].(map[string]any)["type"]; typed {
		t.Fatal("raw JSON field must accept any type")
	}
	labels := props["labels"].(map[string]any)["additionalProperties"].(map[string]any)
	if labels["type"] != "integer" {
		t.Fatalf("labels %v", labels)
	}
	checkRequired(t, doc, "shared", "city", "days")
}

func checkProp(t *testing.T, props map[string]any, name, key string, want any) {
	t.Helper()
	if got := props[name].(map[string]any)[key]; !reflect.DeepEqual(got, want) {
		t.Errorf("%s.%s = %v, want %v", name, key, got, want)
	}
}

func checkRequired(t *testing.T, doc map[string]any, want ...string) {
	t.Helper()
	got := map[string]bool{}
	for _, name := range doc["required"].([]any) {
		got[name.(string)] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("%s not required", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("required %v, want %v", got, want)
	}
}

func TestEmptyStruct(t *testing.T) {
	doc := render(t, compile(t, struct{}{}))
	if _, ok := doc["required"]; ok || len(doc["properties"].(map[string]any)) != 0 {
		t.Fatalf("doc %v", doc)
	}
}

type recursive struct {
	Next *recursive `json:"next"`
}

type indirect struct {
	Children []indirect `json:"children"`
}

func TestRejectedTypes(t *testing.T) {
	cases := []any{
		nil, "string", 3, time.Time{}, &full{},
		struct{ F func() }{}, struct{ C chan int }{}, struct{ I any }{},
		struct{ B []byte }{}, struct{ A [3]int }{}, struct{ M map[int]string }{},
		struct{ P **string }{}, struct{ U uintptr }{}, struct{ X complex64 }{},
		recursive{}, indirect{},
		struct {
			X string
			Y string `json:"X"`
		}{},
		struct{ M map[string]func() }{},
		struct{ S []chan int }{},
		struct{ N struct{ F func() } }{},
	}
	for _, v := range cases {
		if _, err := For(reflect.TypeOf(v)); !errors.HasCode(err, ErrType) {
			t.Errorf("%T accepted: %v", v, err)
		}
	}
}

func TestSiblingReuseIsNotRecursion(t *testing.T) {
	type pair struct {
		A inner `json:"a"`
		B inner `json:"b"`
	}
	if n := compile(t, pair{}); len(n.Fields) != 2 {
		t.Fatalf("fields %v", n.Fields)
	}
}

func TestBadTags(t *testing.T) {
	cases := []any{
		struct {
			N int `json:"n" enum:"1,2"`
		}{},
		struct {
			S string `json:"s" enum:"a,,b"`
		}{},
		struct {
			S string `json:"s" min:"x"`
		}{},
		struct {
			S string `json:"s" max:"Inf"`
		}{},
		struct {
			S string `json:"s" min:"-1"`
		}{},
		struct {
			S []int `json:"s" max:"2.5"`
		}{},
		struct {
			B bool `json:"b" min:"0"`
		}{},
		struct {
			N int `json:"n" min:"5" max:"1"`
		}{},
		struct {
			N int `json:"n" max:"NaN"`
		}{},
	}
	for _, v := range cases {
		if _, err := For(reflect.TypeOf(v)); !errors.HasCode(err, ErrType) {
			t.Errorf("%+v accepted: %v", v, err)
		}
	}
}

func TestErrorNamesTheType(t *testing.T) {
	_, err := For(reflect.TypeOf(struct{ F func() }{}))
	if err == nil || !strings.Contains(err.Error(), "func()") {
		t.Fatalf("err %v", err)
	}
}

type BadEmbedded struct {
	F func() `json:"f"`
}

func TestBadEmbeddedField(t *testing.T) {
	if _, err := For(reflect.TypeOf(struct{ BadEmbedded }{})); !errors.HasCode(err, ErrType) {
		t.Fatalf("err %v", err)
	}
}
