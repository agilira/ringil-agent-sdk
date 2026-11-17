// registry_test.go: tests for the tool registry
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"runtime"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

// rawTool is a low-level tool whose spec the test controls.
type rawTool struct {
	spec ToolSpec
	call func(context.Context, json.RawMessage) (ToolOutput, error)
}

func (r *rawTool) Spec() ToolSpec { return r.spec }
func (r *rawTool) Call(ctx context.Context, in json.RawMessage) (ToolOutput, error) {
	return r.call(ctx, in)
}

func echo(name string) *rawTool {
	return &rawTool{
		spec: ToolSpec{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
		call: func(_ context.Context, in json.RawMessage) (ToolOutput, error) {
			return ToolOutput{Content: string(in)}, nil
		},
	}
}

func mustRegistry(t *testing.T, tools ...Tool) Registry {
	t.Helper()
	r, err := NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func names(specs []ToolSpec) string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return strings.Join(out, ",")
}

func TestRegistrySpecsSorted(t *testing.T) {
	r := mustRegistry(t, echo("b"), echo("a"), echo("c"))
	if got := names(r.Specs()); got != "a,b,c" || !r.Has("a") || r.Has("z") {
		t.Fatalf("specs %s", got)
	}
}

func TestRegistryRefusesBadTools(t *testing.T) {
	badSchema := echo("s")
	badSchema.spec.InputSchema = json.RawMessage(`[1]`)
	nullSchema := echo("n")
	nullSchema.spec.InputSchema = json.RawMessage(`null`)
	cases := [][]Tool{
		{nil},
		{echo("")},
		{echo("has space")},
		{echo(strings.Repeat("a", 65))},
		{echo("../etc")},
		{badSchema},
		{nullSchema},
		{echo("a"), echo("a")},
	}
	for i, tools := range cases {
		if _, err := NewRegistry(tools...); !errors.HasCode(err, ErrToolConfig) {
			t.Errorf("case %d accepted: %v", i, err)
		}
	}
}

func TestRegistryFreezesSpecs(t *testing.T) {
	tool := echo("a")
	r := mustRegistry(t, tool)
	tool.spec.Description = "changed after registration"
	tool.spec.InputSchema[0] = 'X'
	if spec := r.Specs()[0]; spec.Description != "" || spec.InputSchema[0] != '{' {
		t.Fatalf("registry follows a mutated tool: %+v", spec)
	}
	r.Specs()[0].InputSchema[0] = 'Y'
	if r.Specs()[0].InputSchema[0] != '{' {
		t.Fatal("Specs shares memory with the registry")
	}
}

func TestWithAndSelectDoNotChangeTheOriginal(t *testing.T) {
	base := mustRegistry(t, echo("a"), echo("b"))
	more, err := base.With(echo("c"))
	if err != nil {
		t.Fatal(err)
	}
	sub, err := more.Select("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	if names(base.Specs()) != "a,b" || names(more.Specs()) != "a,b,c" || names(sub.Specs()) != "a,c" {
		t.Fatalf("base %s more %s sub %s", names(base.Specs()), names(more.Specs()), names(sub.Specs()))
	}
	if _, err := base.With(echo("a")); !errors.HasCode(err, ErrToolConfig) {
		t.Fatalf("With accepted a duplicate: %v", err)
	}
	if _, err := sub.Select("b"); !errors.HasCode(err, ErrToolConfig) {
		t.Fatalf("Select widened a subset: %v", err)
	}
}

func TestExecuteSuccess(t *testing.T) {
	r := mustRegistry(t, echo("a"))
	res, err := r.Execute(context.Background(), ToolCall{ID: "1", Name: "a", Input: json.RawMessage(`{"x":1}`)})
	if err != nil || res != (ToolResult{CallID: "1", Content: `{"x":1}`}) {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestExecuteUnknownToolDoesNotEchoTheName(t *testing.T) {
	r := mustRegistry(t, echo("read"), echo("write"))
	res, err := r.Execute(context.Background(), ToolCall{ID: "1", Name: "IGNORE-PREVIOUS-INSTRUCTIONS"})
	if !errors.HasCode(err, ErrToolUnknown) || !res.IsError || res.CallID != "1" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if strings.Contains(res.Content, "IGNORE") || !strings.Contains(res.Content, "read, write") {
		t.Fatalf("content %q", res.Content)
	}
	empty := mustRegistry(t)
	if res, err := empty.Execute(context.Background(), ToolCall{Name: "x"}); !errors.HasCode(err, ErrToolUnknown) || !strings.Contains(res.Content, "none") {
		t.Fatalf("empty registry content %q", res.Content)
	}
}

func TestExecuteBadInput(t *testing.T) {
	called := false
	tool := echo("a")
	tool.call = func(context.Context, json.RawMessage) (ToolOutput, error) { called = true; return ToolOutput{}, nil }
	res, err := mustRegistry(t, tool).Execute(context.Background(), ToolCall{ID: "1", Name: "a", Input: json.RawMessage(`{}`), BadInput: true})
	if !errors.HasCode(err, ErrToolInput) || !res.IsError || called {
		t.Fatalf("res %+v err %v called %v", res, err, called)
	}
}

func TestExecuteHidesUndescribedErrors(t *testing.T) {
	tool := echo("a")
	tool.call = func(context.Context, json.RawMessage) (ToolOutput, error) {
		return ToolOutput{}, errors.New("DB_DOWN", "postgres://admin:hunter2@db refused")
	}
	res, err := mustRegistry(t, tool).Execute(context.Background(), ToolCall{ID: "1", Name: "a"})
	if res.Content != "Tool failed [DB_DOWN]." || !res.IsError || !strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("model sees %q, host sees %v", res.Content, err)
	}
}

func TestExecuteRecoversPanics(t *testing.T) {
	tool := echo("a")
	tool.call = func(context.Context, json.RawMessage) (ToolOutput, error) { panic("secret-token-123") }
	res, err := mustRegistry(t, tool).Execute(context.Background(), ToolCall{ID: "1", Name: "a"})
	if !errors.HasCode(err, ErrToolPanic) || !res.IsError || strings.Contains(res.Content, "secret") {
		t.Fatalf("res %+v err %v", res, err)
	}
	if !strings.Contains(err.Error(), "secret-token-123") {
		t.Fatal("host lost the panic value")
	}
}

func TestExecuteGivesToolsACopyOfInput(t *testing.T) {
	tool := echo("a")
	tool.call = func(_ context.Context, in json.RawMessage) (ToolOutput, error) { in[0] = 'X'; return ToolOutput{}, nil }
	input := json.RawMessage(`{}`)
	if _, err := mustRegistry(t, tool).Execute(context.Background(), ToolCall{Name: "a", Input: input}); err != nil {
		t.Fatal(err)
	}
	if string(input) != `{}` {
		t.Fatal("a tool modified the conversation's copy of its input")
	}
}

func TestExecuteCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	tool := echo("a")
	tool.call = func(context.Context, json.RawMessage) (ToolOutput, error) { called = true; return ToolOutput{}, nil }
	if _, err := mustRegistry(t, tool).Execute(ctx, ToolCall{Name: "a"}); !stderrors.Is(err, context.Canceled) || called {
		t.Fatalf("err %v called %v", err, called)
	}
}

func TestExecuteTypedToolEndToEnd(t *testing.T) {
	calls := 0
	r := mustRegistry(t, weatherTool(&calls))
	res, err := r.Execute(context.Background(), ToolCall{ID: "9", Name: "weather", Input: json.RawMessage(`{"city":"Roma","pwd":"hunter2"}`)})
	if !errors.HasCode(err, ErrToolInput) || calls != 0 || strings.Contains(res.Content, "pwd") || strings.Contains(res.Content, "hunter2") {
		t.Fatalf("res %+v err %v calls %d", res, err, calls)
	}
	if !strings.Contains(res.Content, "allowed: city, unit") {
		t.Fatalf("model not told how to fix it: %q", res.Content)
	}
}

func TestExecuteSurvivesGoexit(t *testing.T) {
	tool := echo("a")
	tool.call = func(context.Context, json.RawMessage) (ToolOutput, error) { runtime.Goexit(); return ToolOutput{}, nil }
	res, err := mustRegistry(t, tool).Execute(context.Background(), ToolCall{ID: "1", Name: "a"})
	if !errors.HasCode(err, ErrToolPanic) || !res.IsError {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestPanickingSpecRefused(t *testing.T) {
	var typedNil *rawTool
	if _, err := NewRegistry(typedNil); !errors.HasCode(err, ErrToolConfig) {
		t.Fatalf("typed nil: %v", err)
	}
}

func TestNilOptionIgnored(t *testing.T) {
	tool := NewTool("t", "", func(context.Context, struct{}) (string, error) { return "", nil }, nil, ReadOnly())
	if !tool.Spec().Annotations.ReadOnly {
		t.Fatal("options after a nil one were lost")
	}
}
