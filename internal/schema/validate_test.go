// validate_test.go: tests for argument validation
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func problems(t *testing.T, n *Node, input string) string {
	t.Helper()
	var parts []string
	_, _, got := n.Validate([]byte(input))
	for _, p := range got {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, "; ")
}

const valid = `{"shared":"s","city":"Roma","days":3,"exact":null,"opt":null}`

func TestValidInputs(t *testing.T) {
	n := compile(t, full{})
	inputs := []string{
		valid,
		`{"shared":"s","city":"Roma","days":7,"unit":"f","count":65535,"ratio":0.5,"exact":true,` +
			`"tags":["a","b"],"at":"2026-11-18T09:30:00+01:00","extra":{"any":[1,"x"]},"labels":{"k":1},` +
			`"nested":{"note":"n"},"many":[{},{"note":"x"}],"Untagged":"u","opt":{"k":{}}}`,
	}
	for _, in := range inputs {
		if got := problems(t, n, in); got != "" {
			t.Errorf("%s refused: %s", in, got)
		}
		var decoded full
		if err := json.Unmarshal([]byte(in), &decoded); err != nil {
			t.Errorf("validated input does not decode: %v", err)
		}
	}
}

func TestEmptyAndNullInput(t *testing.T) {
	n := compile(t, struct {
		Opt string `json:"opt,omitempty"`
	}{})
	for _, in := range []string{"", "  ", "null", "{}"} {
		if got := problems(t, n, in); got != "" {
			t.Errorf("%q refused: %s", in, got)
		}
	}
}

func TestRefusals(t *testing.T) {
	n := compile(t, full{})
	cases := map[string]string{
		`{"city":"Roma","days":3}`:          "shared: is required",
		`not json`:                          "arguments: must be one JSON object",
		`{} {}`:                             "arguments: must be one JSON object",
		`[1]`:                               "arguments: must be an object",
		`{"shared":"s","city":5,"days":3}`:  "city: must be a string",
		`{"shared":"s","city":"","days":3}`: "city: must be between 1 and 80 characters long",
		`{"shared":"s","city":"R","days":3,"unit":"k"}`:               "unit: must be one of: c, f",
		`{"shared":"s","city":"R","days":0}`:                          "days: must be between 1 and 7",
		`{"shared":"s","city":"R","days":1.5}`:                        "days: must be an integer that fits 8 bits",
		`{"shared":"s","city":"R","days":300}`:                        "days: must be an integer that fits 8 bits",
		`{"shared":"s","city":"R","days":"3"}`:                        "days: must be a number",
		`{"shared":"s","city":"R","days":1,"count":-1}`:               "count: must be a non-negative integer that fits 16 bits",
		`{"shared":"s","city":"R","days":1,"ratio":2}`:                "ratio: must be between 0 and 1",
		`{"shared":"s","city":"R","days":1,"ratio":1e40}`:             "ratio: must be a finite number",
		`{"shared":"s","city":"R","days":1,"exact":"yes"}`:            "exact: must be a boolean",
		`{"shared":"s","city":"R","days":1,"tags":["a","b","c","d"]}`: "tags: must have at most 3 items",
		`{"shared":"s","city":"R","days":1,"tags":"a"}`:               "tags: must be an array",
		`{"shared":"s","city":"R","days":1,"tags":[1]}`:               "tags[0]: must be a string",
		`{"shared":"s","city":"R","days":1,"at":"tomorrow"}`:          "at: must be an RFC 3339 date-time, e.g. 2026-11-18T09:30:00Z",
		`{"shared":"s","city":"R","days":1,"labels":{"k":"v"}}`:       "labels.*: must be a number",
		`{"shared":"s","city":"R","days":1,"labels":[]}`:              "labels: must be an object",
		`{"shared":"s","city":"R","days":1,"nested":{"note":1}}`:      "nested.note: must be a string",
		`{"shared":"s","city":null,"days":1}`:                         "city: must not be null",
	}
	for in, want := range cases {
		if got := problems(t, n, in); got != want {
			t.Errorf("%s\n got: %s\nwant: %s", in, got, want)
		}
	}
}

func TestCaseVariantsAreUnknown(t *testing.T) {
	// encoding/json would accept "CITY" for "city"; the validator does not,
	// so a field cannot be smuggled past the schema by changing its case.
	n := compile(t, full{})
	canonical, ignored, probs := n.Validate([]byte(`{"shared":"s","city":"R","CITY":"X","days":1}`))
	if len(probs) > 0 || ignored != 1 || strings.Contains(string(canonical), "CITY") || strings.Contains(string(canonical), `"X"`) {
		t.Fatalf("case variant reached the canonical form: %s %d %v", canonical, ignored, probs)
	}
}

func TestProblemsAreBounded(t *testing.T) {
	type many struct {
		A, B, C, D, E, F, G, H, I, J, K string
	}
	_, _, got := compile(t, many{}).Validate([]byte(`{}`))
	if len(got) != maxProblems+1 || got[maxProblems].Message != "3 more problems not listed" {
		t.Fatalf("problems %v", got)
	}
}

func TestNumberBoundaries(t *testing.T) {
	type nums struct {
		I64 int64   `json:"i64"`
		U64 uint64  `json:"u64"`
		F64 float64 `json:"f64"`
	}
	n := compile(t, nums{})
	ok := `{"i64":-9223372036854775808,"u64":18446744073709551615,"f64":1.7976931348623157e308}`
	if got := problems(t, n, ok); got != "" {
		t.Fatalf("boundaries refused: %s", got)
	}
	bad := `{"i64":9223372036854775808,"u64":18446744073709551616,"f64":1e309}`
	if got := problems(t, n, bad); strings.Count(got, "must be") != 3 {
		t.Fatalf("overflow accepted: %s", got)
	}
}

func TestRangeText(t *testing.T) {
	lo, hi := &Bound{text: "1", f: 1}, &Bound{text: "2.5", f: 2.5}
	cases := map[string]*Node{
		"between 1 and 2.5": {Min: lo, Max: hi},
		"at least 1":        {Min: lo},
		"at most 2.5":       {Max: hi},
		"valid":             {},
	}
	for want, n := range cases {
		if got := rangeText(n); got != want {
			t.Errorf("rangeText = %q, want %q", got, want)
		}
	}
}

// secret is a marker that must never appear in a problem report.
const secret = "SECRET-VALUE-7f3a"

func TestProblemsNeverEchoInput(t *testing.T) {
	n := compile(t, full{})
	inputs := []string{
		`{"` + secret + `":1,"shared":"s","city":"R","days":1}`,
		`{"shared":"s","city":"R","days":1,"unit":"` + secret + `"}`,
		`{"shared":"s","city":"R","days":"` + secret + `"}`,
		`{"shared":"s","city":"R","days":1,"labels":{"` + secret + `":"x"}}`,
		`{"shared":"s","city":"R","days":1,"at":"` + secret + `"}`,
		`{"shared":"s","city":"R","days":1,"nested":{"` + secret + `":1}}`,
	}
	for _, in := range inputs {
		canonical, _, probs := n.Validate([]byte(in))
		got := fmt.Sprint(probs)
		// Either refused without echo, or accepted with the unknown field
		// dropped from what the tool decodes.
		if strings.Contains(got, secret) || (len(probs) == 0 && strings.Contains(string(canonical), secret)) {
			t.Errorf("input %s produced %q / %s", in, got, canonical)
		}
	}
}

// FuzzValidate checks the core promise: input that validates always decodes
// into the tool's type, and no report ever repeats the input.
func FuzzValidate(f *testing.F) {
	f.Add([]byte(valid))
	f.Add([]byte(`{"shared":"s","city":"R","days":1,"tags":["x"],"labels":{"a":1},"many":[{"note":"n"}]}`))
	f.Add([]byte(`{"days":1e400,"ratio":-0,"count":0.0}`))
	f.Add([]byte(`null`))
	n, err := For(reflectFull)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		canonical, _, got := n.Validate(data)
		if len(got) == 0 {
			var decoded full
			if err := json.Unmarshal(canonical, &decoded); err != nil {
				t.Fatalf("validated input does not decode: %v\n%s", err, data)
			}
			return
		}
		checkNoEcho(t, got)
	})
}

var reflectFull = reflect.TypeOf(full{})

// schemaPath matches paths built only from the field names of full, array
// indexes and the map wildcard.
var schemaPath = regexp.MustCompile(`^(arguments|[A-Za-z]+)(\.\*|\[[0-9]+\]|\.[A-Za-z]+)*$`)

func checkNoEcho(t *testing.T, got []Problem) {
	t.Helper()
	if len(got) > maxProblems+1 {
		t.Fatalf("%d problems", len(got))
	}
	for _, p := range got {
		if !schemaPath.MatchString(p.Path) || strings.ContainsAny(p.Message, "\"{}\\") {
			t.Fatalf("problem not built from the schema: %q", p)
		}
	}
}

func TestNumberParseFloat32Range(t *testing.T) {
	type f32 struct {
		F float32 `json:"f"`
	}
	n := compile(t, f32{})
	if got := problems(t, n, `{"f":3.5e38}`); got == "" {
		t.Fatal("float32 overflow accepted")
	}
	if got := problems(t, n, `{"f":3.4e38}`); got != "" {
		t.Fatalf("float32 max refused: %s", got)
	}
}

// 18/11: unknown fields are dropped and counted, at any depth: a small
// model sending {"item":"AX-7"} to a tool without parameters looped on
// the refusal.
func TestUnknownFieldsAreDropped(t *testing.T) {
	canonical, ignored, probs := compile(t, struct{}{}).Validate([]byte(`{"x":1,"y":{"z":2}}`))
	if len(probs) > 0 || ignored != 2 || string(canonical) != "{}" {
		t.Fatalf("got %s %d %v", canonical, ignored, probs)
	}
	canonical, ignored, probs = compile(t, full{}).Validate([]byte(`{"shared":"s","city":"R","days":1,"many":[{"x":1,"note":"n"}]}`))
	if len(probs) > 0 || ignored != 1 || strings.Contains(string(canonical), `"x"`) {
		t.Fatalf("nested: %s %d %v", canonical, ignored, probs)
	}
}

// 18/11: Qwen 3B sent "limit":null and was refused; null for an optional
// field is the field left out. A required field still refuses null.
func TestNullOptionalIsAbsent(t *testing.T) {
	canonical, _, probs := compile(t, full{}).Validate([]byte(`{"shared":"s","city":"R","days":1,"unit":null}`))
	if len(probs) > 0 || strings.Contains(string(canonical), "unit") {
		t.Fatalf("%s %v", canonical, probs)
	}
	if got := problems(t, compile(t, full{}), `{"shared":"s","city":null,"days":1}`); got != "city: must not be null" {
		t.Fatalf("required null: %s", got)
	}
}
