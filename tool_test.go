// tool_test.go: tests for typed tools and model-facing errors
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

type weatherIn struct {
	City string `json:"city" desc:"City name" min:"1"`
	Unit string `json:"unit,omitempty" enum:"c,f"`
}

type weatherOut struct {
	City string  `json:"city"`
	Temp float64 `json:"temp"`
}

func weatherTool(calls *int) Tool {
	return NewTool("weather", "Current temperature", func(_ context.Context, in weatherIn) (weatherOut, error) {
		*calls++
		return weatherOut{City: in.City, Temp: 21.5}, nil
	}, ReadOnly(), OpenWorld())
}

func TestNewToolSpec(t *testing.T) {
	spec := weatherTool(new(int)).Spec()
	if spec.Name != "weather" || spec.Description != "Current temperature" ||
		!spec.Annotations.ReadOnly || !spec.Annotations.OpenWorld || spec.Annotations.Destructive {
		t.Fatalf("spec %+v", spec)
	}
	var doc map[string]any
	if err := json.Unmarshal(spec.InputSchema, &doc); err != nil {
		t.Fatal(err)
	}
	props := doc["properties"].(map[string]any)
	if doc["type"] != "object" || props["city"].(map[string]any)["description"] != "City name" {
		t.Fatalf("schema %s", spec.InputSchema)
	}
}

func TestSpecIsACopy(t *testing.T) {
	tool := weatherTool(new(int))
	spec := tool.Spec()
	spec.InputSchema[0] = 'X'
	if tool.Spec().InputSchema[0] != '{' {
		t.Fatal("Spec shares the schema buffer")
	}
}

func TestNewToolCallStructuredOutput(t *testing.T) {
	calls := 0
	out, err := weatherTool(&calls).Call(context.Background(), json.RawMessage(`{"city":"Roma"}`))
	if err != nil || out.Content != `{"city":"Roma","temp":21.5}` || calls != 1 {
		t.Fatalf("out %+v err %v calls %d", out, err, calls)
	}
}

func TestNewToolOutputs(t *testing.T) {
	str := NewTool("s", "", func(context.Context, struct{}) (string, error) { return "plain", nil })
	raw := NewTool("o", "", func(context.Context, struct{}) (ToolOutput, error) { return ToolOutput{Content: "given"}, nil })
	for tool, want := range map[Tool]string{str: "plain", raw: "given"} {
		if out, err := tool.Call(context.Background(), nil); err != nil || out.Content != want {
			t.Errorf("%s: %+v %v", tool.Spec().Name, out, err)
		}
	}
}

func TestNewToolUnencodableOutput(t *testing.T) {
	tool := NewTool("c", "", func(context.Context, struct{}) (chan int, error) { return make(chan int), nil })
	if _, err := tool.Call(context.Background(), nil); !errors.HasCode(err, ErrToolFailed) {
		t.Fatalf("err %v", err)
	}
}

func TestNewToolRefusesBeforeCalling(t *testing.T) {
	calls := 0
	tool := weatherTool(&calls)
	for _, in := range []string{`{}`, `{"city":""}`, `{"city":"R","unit":"k"}`, `{"city":"R","x":1}`, `{"City":"R"}`, `[]`, `{"city":1}`} {
		_, err := tool.Call(context.Background(), json.RawMessage(in))
		if !errors.HasCode(err, ErrToolInput) || !strings.HasPrefix(ModelMessage(err), "Invalid arguments: ") {
			t.Errorf("%s: %v / %s", in, err, ModelMessage(err))
		}
	}
	if calls != 0 {
		t.Fatalf("function ran %d times on refused input", calls)
	}
}

func TestNewToolErrorPassesThrough(t *testing.T) {
	boom := ToolError("NOT_FOUND", "City not found.")
	tool := NewTool("w", "", func(context.Context, weatherIn) (string, error) { return "", boom })
	_, err := tool.Call(context.Background(), json.RawMessage(`{"city":"Atlantide"}`))
	if !stderrors.Is(err, boom) || ModelMessage(err) != "City not found. [NOT_FOUND]" {
		t.Fatalf("err %v msg %q", err, ModelMessage(err))
	}
}

func TestInvalidToolsReportAtRegistration(t *testing.T) {
	bad := []Tool{
		NewTool[weatherIn, string]("nilfn", "", nil),
		NewTool("notstruct", "", func(context.Context, string) (string, error) { return "", nil }),
		NewTool("badtype", "", func(context.Context, struct{ F func() }) (string, error) { return "", nil }),
	}
	for _, tool := range bad {
		if _, err := NewRegistry(tool); !errors.HasCode(err, ErrToolConfig) {
			t.Errorf("%s registered: %v", tool.Spec().Name, err)
		}
		if _, err := tool.Call(context.Background(), nil); !errors.HasCode(err, ErrToolConfig) {
			t.Errorf("%s callable: %v", tool.Spec().Name, err)
		}
	}
}

func TestDestructiveOption(t *testing.T) {
	tool := NewTool("rm", "", func(context.Context, struct{}) (string, error) { return "", nil }, Destructive())
	if !tool.Spec().Annotations.Destructive {
		t.Fatal("Destructive not applied")
	}
}

func TestModelMessage(t *testing.T) {
	inner := ToolError("QUOTA", "Quota exceeded.")
	cases := map[error]string{
		nil:                                  "Tool failed.",
		stderrors.New("/home/user/.ssh/key"): "Tool failed.",
		errors.New("DB_DOWN", "conn to postgres://u:pw@db failed"):     "Tool failed [DB_DOWN].",
		errors.New("bad code!", "x"):                                   "Tool failed.",
		errors.Wrap(inner, "OUTER", "context"):                         "Quota exceeded. [QUOTA]",
		fmt.Errorf("wrapped: %w", inner):                               "Quota exceeded. [QUOTA]",
		errors.Wrap(stderrors.New("secret"), "WRAPPED", "with secret"): "Tool failed [WRAPPED].",
	}
	for err, want := range cases {
		if got := ModelMessage(err); got != want {
			t.Errorf("ModelMessage(%v) = %q, want %q", err, got, want)
		}
	}
}

// FuzzNewToolCall drives a typed tool with arbitrary arguments: it must
// never panic, and the function must only ever see input that matches.
func FuzzNewToolCall(f *testing.F) {
	f.Add([]byte(`{"city":"Roma","unit":"c"}`))
	f.Add([]byte(`{"city":"","unit":"x","extra":[1,2]}`))
	f.Add([]byte(`null`))
	tool := NewTool("weather", "", func(_ context.Context, in weatherIn) (string, error) {
		if in.City == "" || (in.Unit != "" && in.Unit != "c" && in.Unit != "f") {
			return "", stderrors.New("function saw invalid input")
		}
		return in.City, nil
	})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, err := tool.Call(context.Background(), data)
		if err != nil && !errors.HasCode(err, ErrToolInput) {
			t.Fatalf("unexpected failure %v for %q", err, data)
		}
	})
}
