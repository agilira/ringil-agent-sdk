// audit_test.go: regressions for the findings of the 18/11 security audit
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package schema

import (
	"encoding/json"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"
)

type guarded struct {
	B string `json:"b,omitempty" enum:"ok"`
	N int    `json:"n,omitempty" max:"10"`
}

type dupHolder struct {
	In guarded            `json:"in,omitzero"`
	P  *guarded           `json:"p"`
	M  map[string]guarded `json:"m,omitempty"`
	A  []guarded          `json:"a,omitempty" max:"1"`
}

// decodeValidated validates input and decodes the canonical form, as tools do.
func decodeValidated(t *testing.T, n *Node, input string, out any) bool {
	t.Helper()
	canonical, problems := n.Validate([]byte(input))
	if len(problems) > 0 {
		return false
	}
	if err := json.Unmarshal(canonical, out); err != nil {
		t.Fatalf("canonical form does not decode: %v", err)
	}
	return true
}

// Finding 1: duplicate keys merged by encoding/json bypassed enum and max.
func TestDuplicateKeysCannotSmuggleValues(t *testing.T) {
	n := compile(t, dupHolder{})
	inputs := []string{
		`{"in":{"b":"EVIL","n":999},"in":{}}`,
		`{"p":{"b":"EVIL"},"p":{}}`,
		`{"m":{"k":{"b":"EVIL"}},"m":{}}`,
		`{"a":[{"b":"EVIL"},{}],"a":[{}]}`,
	}
	for _, in := range inputs {
		var got dupHolder
		if !decodeValidated(t, n, in, &got) {
			continue
		}
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "EVIL") || strings.Contains(string(data), "999") || len(got.A) > 1 {
			t.Errorf("%s reached the tool as %s", in, data)
		}
	}
}

// Finding 2: embedded pointer structs disagreed with encoding/json.
type Inner struct {
	B string `json:"b"`
}

func TestEmbeddedPointerRefused(t *testing.T) {
	type embPtr struct {
		*Inner
		X int `json:"x"`
	}
	if _, err := For(reflect.TypeOf(embPtr{})); !errors.HasCode(err, ErrType) {
		t.Fatalf("err %v", err)
	}
}

// Finding 3: tag names encoding/json ignores made schema and struct disagree.
func TestInvalidTagNameRefused(t *testing.T) {
	type quoted struct {
		//lint:ignore SA5008 the malformed name is what is being tested
		Its int `json:"it's" min:"1"`
	}
	if _, err := For(reflect.TypeOf(quoted{})); !errors.HasCode(err, ErrType) {
		t.Fatalf("err %v", err)
	}
	type fine struct {
		A int `json:"a-b.c:d"`
	}
	compile(t, fine{})
}

// Finding 4: ,string and custom decoders validated but never decoded.
func TestUndecodableTypesRefused(t *testing.T) {
	cases := []any{
		struct {
			N int `json:"n,string"`
		}{},
		struct {
			time.Time
			X int `json:"x"`
		}{},
		struct {
			B big.Int `json:"b"`
		}{},
		struct {
			D customText `json:"d"`
		}{},
	}
	for _, v := range cases {
		if _, err := For(reflect.TypeOf(v)); !errors.HasCode(err, ErrType) {
			t.Errorf("%T accepted: %v", v, err)
		}
	}
}

type customText string

func (c *customText) UnmarshalText([]byte) error { return nil }

// Finding 5: int64 bounds above 2^53 were compared as float64.
func TestLargeIntegerBoundsAreExact(t *testing.T) {
	type big64 struct {
		N int64  `json:"n" max:"9007199254740992"`
		U uint64 `json:"u" min:"18446744073709551614"`
	}
	n := compile(t, big64{})
	if got := problems(t, n, `{"n":9007199254740993,"u":18446744073709551615}`); got != "n: must be at most 9007199254740992" {
		t.Fatalf("got %q", got)
	}
	if got := problems(t, n, `{"n":9007199254740992,"u":18446744073709551613}`); got != "u: must be at least 18446744073709551614" {
		t.Fatalf("got %q", got)
	}
}

// Finding 6: float32 values were compared to bounds after rounding.
func TestFloat32BoundsUseTheWrittenValue(t *testing.T) {
	type f32 struct {
		F float32 `json:"f" min:"0.1" max:"0.1"`
	}
	if got := problems(t, compile(t, f32{}), `{"f":0.1}`); got != "" {
		t.Fatalf("0.1 refused: %s", got)
	}
}

// Plausible finding: offsets time.Time cannot marshal again.
func TestDateTimeMustRoundTrip(t *testing.T) {
	type when struct {
		At time.Time `json:"at"`
	}
	n := compile(t, when{})
	if got := problems(t, n, `{"at":"2026-11-18T10:00:00+24:00"}`); got == "" {
		t.Fatal("+24:00 offset accepted")
	}
	if got := problems(t, n, `{"at":"2026-11-18T10:00:00+01:00"}`); got != "" {
		t.Fatalf("valid date refused: %s", got)
	}
}

func TestIntegerBoundTags(t *testing.T) {
	for _, v := range []any{
		struct {
			N int `json:"n" min:"1.5"`
		}{},
		struct {
			N int `json:"n" min:"1e3"`
		}{},
	} {
		if _, err := For(reflect.TypeOf(v)); !errors.HasCode(err, ErrType) {
			t.Errorf("%+v accepted: %v", v, err)
		}
	}
}
