// structured_test.go: answers in JSON a schema describes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

type invoice struct {
	Number string  `json:"number" min:"3" desc:"the invoice number"`
	Total  float64 `json:"total" min:"0"`
	Lines  []line  `json:"lines"`
	Note   *string `json:"note,omitempty"`
}

type line struct {
	Item string `json:"item"`
	Qty  int    `json:"qty" min:"1"`
}

// The agent sends the format with every model call, and Parse decodes the
// answer after checking it, bounds included, a fence taken off.
func TestStructuredAnswer(t *testing.T) {
	f, err := agent.FormatFor[invoice]("invoice")
	if err != nil {
		t.Fatal(err)
	}
	p := fake.New(fake.Reply("```json\n" + `{"number":"F-7","total":12.5,"lines":[{"item":"pen","qty":2}]}` + "\n```"))
	res, err := newAgent(t, agent.Config{Provider: p, Format: f}).Run(context.Background(), nil, agent.UserText("read the invoice"))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Requests()[0].Format; !reflect.DeepEqual(got, f) {
		t.Fatalf("the request did not carry the format: %v", got)
	}
	inv, err := agent.Parse[invoice](f, res.Text)
	if err != nil || inv.Number != "F-7" || inv.Lines[0].Qty != 2 || inv.Note != nil {
		t.Fatalf("%+v %v", inv, err)
	}
}

// The schema providers get has no bounds (Parse checks them), and is not
// strict when a field is optional.
func TestFormatSchema(t *testing.T) {
	f, _ := agent.FormatFor[invoice]("invoice")
	if strings.Contains(string(f.Schema), "minLength") || f.Strict {
		t.Fatalf("schema %s strict %v", f.Schema, f.Strict)
	}
}

// What does not match is an error naming fields, never the answer.
func TestParseRefusesWhatDoesNotMatch(t *testing.T) {
	f, _ := agent.FormatFor[invoice]("invoice")
	for _, answer := range []string{
		`{"number":"F","total":1,"lines":[]}`,                          // too short
		`{"number":"F-7","total":-1,"lines":[]}`,                       // below the minimum
		`{"number":"F-7","total":"12","lines":[]}`,                     // a string for a number
		`{"number":"F-7","lines":[{"item":"x","qty":0}]}`,              // no total, qty 0
		`not json SECRET-TEXT`,                                         // not JSON at all
		`{"number":"SECRET-TEXT-123","total":1,"lines":[{"qty":"x"}]}`, // a value in the answer
	} {
		_, err := agent.Parse[invoice](f, answer)
		if !errors.HasCode(err, agent.ErrFormat) || strings.Contains(err.Error(), "SECRET-TEXT") {
			t.Errorf("%s: %v", answer, err)
		}
	}
}

// Audit 23/11: an empty, null or prose answer is no empty object, and a
// field the type does not define is refused, not dropped.
func TestParseRefusesNothingAndExtras(t *testing.T) {
	type loose struct {
		Note *string `json:"note,omitempty"`
	}
	f, _ := agent.FormatFor[loose]("loose")
	for _, answer := range []string{"", "  ", "null", "```\n```", "sure, here it is", `{"Note":"x"}`} {
		if _, err := agent.Parse[loose](f, answer); !errors.HasCode(err, agent.ErrFormat) {
			t.Errorf("%q: %v", answer, err)
		}
	}
	if v, err := agent.Parse[loose](f, `{}`); err != nil || v.Note != nil {
		t.Fatalf("%+v %v", v, err)
	}
	type nested struct {
		In struct {
			X *int `json:"x,omitempty"`
		} `json:"in"`
	}
	if g, _ := agent.FormatFor[nested]("nested"); g.Strict {
		t.Error("an optional field deep down was taken for strict")
	}
	if _, err := agent.FormatFor[struct {
		Raw json.RawMessage `json:"raw"`
	}]("raw"); err == nil {
		t.Error("raw JSON has no type to give")
	}
}

func TestFormatForRefuses(t *testing.T) {
	if _, err := agent.FormatFor[invoice]("bad name"); err == nil {
		t.Error("a bad name")
	}
	if _, err := agent.FormatFor[[]line]("list"); err == nil {
		t.Error("a list is no object")
	}
	if _, err := agent.FormatFor[struct {
		M map[string]int `json:"m"`
	}]("map"); err == nil {
		t.Error("a map has no portable form")
	}
	if _, err := agent.Parse[invoice](&agent.Format{Name: "x"}, `{}`); err == nil {
		t.Error("a Format not made by FormatFor")
	}
	type closed struct {
		A string `json:"a"`
	}
	if f, err := agent.FormatFor[closed]("closed"); err != nil || !f.Strict {
		t.Fatalf("%+v %v", f, err)
	}
}

func TestParseTypeMismatch(t *testing.T) {
	f, err := agent.FormatFor[invoice]("invoice")
	if err != nil {
		t.Fatal(err)
	}
	type wrong struct {
		Number time.Time `json:"number"`
	}
	_, err = agent.Parse[wrong](f, `{"number":"SECRET-TEXT-bad-date","total":1,"lines":[]}`)
	if !errors.HasCode(err, agent.ErrFormat) || strings.Contains(err.Error(), "SECRET-TEXT") {
		t.Fatalf("mismatched type not refused cleanly: %v", err)
	}
}

func TestFormatForChecks(t *testing.T) {
	for _, err := range []error{
		errOf(agent.FormatFor[[]line]("list")),
		errOf(agent.FormatFor[time.Time]("when")),
		errOf(agent.FormatFor[any]("any")),
	} {
		if !errors.HasCode(err, agent.ErrFormat) || !strings.Contains(err.Error(), "give a struct") {
			t.Errorf("expected give-a-struct error, got %v", err)
		}
	}
	type stepWrap struct {
		Step struct {
			Arguments string `json:"arguments" min:"3"`
		} `json:"step"`
	}
	f, err := agent.FormatFor[stepWrap]("wrap")
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Parse[stepWrap](f, `{"step":{"arguments":"ab"}}`)
	if !errors.HasCode(err, agent.ErrFormat) || !strings.Contains(err.Error(), "step.arguments:") {
		t.Fatalf("path corrupted: %v", err)
	}
	if _, err := agent.New(agent.Config{Provider: fake.New(), Format: &agent.Format{Name: "x"}}); !errors.HasCode(err, agent.ErrAgentConfig) {
		t.Fatalf("expected ErrAgentConfig, got %v", err)
	}
}

func errOf(_ *agent.Format, err error) error { return err }
