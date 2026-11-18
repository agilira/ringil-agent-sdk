// tool_func.go: tools built from typed Go functions
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/schema"
)

// NewTool builds a tool from a typed function. The input schema is derived
// from In, which must be a struct:
//
//	type WeatherIn struct {
//	    City string `json:"city" desc:"City name"`
//	    Unit string `json:"unit,omitempty" enum:"c,f"`
//	}
//	weather := agent.NewTool("weather", "Current temperature in a city",
//	    func(ctx context.Context, in WeatherIn) (string, error) { ... },
//	    agent.ReadOnly())
//
// Tags: json (name, omitempty, omitzero), desc, enum (strings), min and max
// (value of numbers, length of strings, items of arrays). A field is
// required unless it is a pointer or tagged omitempty/omitzero.
//
// Before fn runs, the arguments are checked against the schema: unknown
// fields (including case variants), wrong types, out-of-range numbers and
// missing required fields are refused, and the model is told which fields
// are wrong without its values being repeated back.
//
// A string result is returned as is, a [ToolOutput] as given, anything else
// as JSON. NewTool never panics: a type it cannot describe makes the tool
// invalid, and [NewRegistry] reports it.
func NewTool[In, Out any](name, description string, fn func(context.Context, In) (Out, error), opts ...ToolOption) Tool {
	t := &funcTool[In, Out]{fn: fn, spec: ToolSpec{Name: name, Description: description}}
	for _, opt := range opts {
		if opt != nil {
			opt(&t.spec)
		}
	}
	t.err = t.compile()
	return t
}

type funcTool[In, Out any] struct {
	spec ToolSpec
	node *schema.Node
	fn   func(context.Context, In) (Out, error)
	err  error
}

// invalidTool is implemented by tools that may have been built badly, so
// the registry can refuse them before the model ever sees them.
type invalidTool interface {
	configError() error
}

func (t *funcTool[In, Out]) configError() error { return t.err }

func (t *funcTool[In, Out]) compile() error {
	if t.fn == nil {
		return errors.New(ErrToolConfig, "tool "+t.spec.Name+": nil function")
	}
	node, err := schema.For(reflect.TypeFor[In]())
	if err != nil {
		return errors.Wrap(err, ErrToolConfig, "tool "+t.spec.Name+": input type")
	}
	doc, err := node.JSON()
	if err != nil {
		return errors.Wrap(err, ErrToolConfig, "tool "+t.spec.Name+": schema")
	}
	t.node, t.spec.InputSchema = node, doc
	return nil
}

func (t *funcTool[In, Out]) Spec() ToolSpec {
	spec := t.spec
	spec.InputSchema = bytes.Clone(t.spec.InputSchema)
	return spec
}

func (t *funcTool[In, Out]) Call(ctx context.Context, input json.RawMessage) (ToolOutput, error) {
	if t.err != nil {
		return ToolOutput{}, t.err
	}
	in, ignored, err := t.decode(input)
	if err != nil {
		return ToolOutput{}, err
	}
	out, err := t.fn(ctx, in)
	if err != nil {
		return ToolOutput{}, err
	}
	res, err := encodeOutput(out)
	if err == nil && ignored > 0 {
		// Said, not hidden: a misspelled optional field was not used.
		res.Content += "\n[" + strconv.Itoa(ignored) + " argument(s) this tool does not define were ignored." + fields(t.spec, "Its parameters:") + "]"
	}
	return res, err
}

func (t *funcTool[In, Out]) decode(input json.RawMessage) (In, int, error) {
	var in In
	canonical, ignored, problems := t.node.Validate(input)
	if len(problems) > 0 {
		parts := make([]string, len(problems))
		for i, p := range problems {
			parts[i] = p.String()
		}
		return in, 0, ToolError(ErrToolInput, "Invalid arguments: "+strings.Join(parts, "; ")+"."+fields(t.spec, "Call "+t.spec.Name+" again with:"))
	}
	// Decode what was validated, not the original bytes: see Validate.
	if err := json.Unmarshal(canonical, &in); err != nil {
		// Validation guarantees this does not happen; if it ever does, the
		// model is told its input was refused, not why.
		return in, 0, errors.Wrap(err, ErrToolInput, "validated input did not decode")
	}
	return in, ignored, nil
}

func encodeOutput(out any) (ToolOutput, error) {
	switch v := out.(type) {
	case string:
		return ToolOutput{Content: v}, nil
	case ToolOutput:
		return v, nil
	}
	data, err := json.Marshal(out)
	if err != nil {
		return ToolOutput{}, errors.Wrap(err, ErrToolFailed, "tool result does not encode as JSON")
	}
	return ToolOutput{Content: string(data)}, nil
}

// fields reminds the model of the tool's parameters after lead: a small
// model often answers a schema error by giving up, unless told what to
// send.
func fields(spec ToolSpec, lead string) string {
	var sch struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if json.Unmarshal(spec.InputSchema, &sch) != nil || len(sch.Properties) == 0 {
		return ""
	}
	names := make([]string, 0, len(sch.Properties))
	for n := range sch.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	req := map[string]bool{}
	for _, r := range sch.Required {
		req[r] = true
	}
	var b strings.Builder
	b.WriteString(" " + lead)
	for _, n := range names {
		mark := "optional"
		if req[n] {
			mark = "required"
		}
		b.WriteString(" " + n + " (" + mark)
		if d := firstLine(sch.Properties[n].Description, 80); d != "" {
			b.WriteString(": " + d)
		}
		b.WriteString(");")
	}
	return strings.TrimSuffix(b.String(), ";") + "."
}

// canonical is the input as the function receives it, encoded again:
// unknown fields and nulls dropped, defaults written out or left out
// alike. Two calls with equal canonical forms do the same thing; the loop
// detector compares these.
func (t *funcTool[In, Out]) canonical(input json.RawMessage) ([]byte, bool) {
	if t.err != nil {
		return nil, false
	}
	in, _, err := t.decode(input)
	if err != nil {
		return nil, false
	}
	data, err := json.Marshal(in)
	return data, err == nil
}
