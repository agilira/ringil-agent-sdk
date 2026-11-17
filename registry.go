// registry.go: the set of tools granted to an agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/agilira/go-errors"
)

// Registry is an immutable set of tools. The host decides what goes in;
// nothing the model says can add a tool or widen a selection.
type Registry interface {
	// Specs returns the tool specs, sorted by name.
	Specs() []ToolSpec
	// Has reports whether name is registered.
	Has(name string) bool
	// With returns a new registry with tools added.
	With(tools ...Tool) (Registry, error)
	// Select returns a new registry holding only the named tools, e.g. the
	// subset granted to a worker.
	Select(names ...string) (Registry, error)
	// Execute runs one tool call and returns the result for the model.
	// The error, when not nil, is the full failure for the host; the
	// result already holds the part the model may see.
	Execute(ctx context.Context, call ToolCall) (ToolResult, error)
}

var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// NewRegistry returns a registry holding tools. A tool with an invalid name
// or schema, a duplicate name or a tool built badly by [NewTool] is refused.
func NewRegistry(tools ...Tool) (Registry, error) {
	r := &registry{entries: map[string]entry{}}
	if err := r.add(tools); err != nil {
		return nil, err
	}
	return r, nil
}

type entry struct {
	spec ToolSpec
	tool Tool
}

type registry struct {
	entries map[string]entry
}

func (r *registry) add(tools []Tool) error {
	for _, tool := range tools {
		e, err := freeze(tool)
		if err != nil {
			return err
		}
		if _, dup := r.entries[e.spec.Name]; dup {
			return errors.New(ErrToolConfig, "duplicate tool name "+e.spec.Name)
		}
		r.entries[e.spec.Name] = e
	}
	return nil
}

// freeze validates a tool and keeps a private copy of its spec, so a tool
// that changes its Spec later cannot change what the model was offered.
func freeze(tool Tool) (e entry, err error) {
	defer func() {
		// A typed nil or a broken Spec is host code failing: report it.
		if v := recover(); v != nil {
			e, err = entry{}, errors.New(ErrToolConfig, fmt.Sprintf("tool spec panicked: %v", v))
		}
	}()
	if tool == nil {
		return entry{}, errors.New(ErrToolConfig, "nil tool")
	}
	if bad, ok := tool.(invalidTool); ok && bad.configError() != nil {
		return entry{}, bad.configError()
	}
	spec := tool.Spec()
	if !toolName.MatchString(spec.Name) {
		return entry{}, errors.New(ErrToolConfig, "tool name must be 1-64 letters, digits, _ or -")
	}
	var schema map[string]any
	if err := json.Unmarshal(spec.InputSchema, &schema); err != nil || schema == nil {
		return entry{}, errors.New(ErrToolConfig, "tool "+spec.Name+": input schema must be a JSON object")
	}
	spec.InputSchema = bytes.Clone(spec.InputSchema)
	return entry{spec: spec, tool: tool}, nil
}

func (r *registry) Specs() []ToolSpec {
	out := make([]ToolSpec, 0, len(r.entries))
	for _, e := range r.entries {
		spec := e.spec
		spec.InputSchema = bytes.Clone(e.spec.InputSchema)
		out = append(out, spec)
	}
	slices.SortFunc(out, func(a, b ToolSpec) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (r *registry) Has(name string) bool {
	_, ok := r.entries[name]
	return ok
}

func (r *registry) clone() *registry {
	c := &registry{entries: make(map[string]entry, len(r.entries))}
	for name, e := range r.entries {
		c.entries[name] = e
	}
	return c
}

func (r *registry) With(tools ...Tool) (Registry, error) {
	c := r.clone()
	if err := c.add(tools); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *registry) Select(names ...string) (Registry, error) {
	c := &registry{entries: make(map[string]entry, len(names))}
	for _, name := range names {
		e, ok := r.entries[name]
		if !ok {
			return nil, errors.New(ErrToolConfig, "selected tool "+name+" is not in the registry")
		}
		c.entries[name] = e
	}
	return c, nil
}

func (r *registry) Execute(ctx context.Context, call ToolCall) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	e, ok := r.entries[call.Name]
	if !ok {
		// The model's tool name is not repeated: it is model output.
		msg := "Unknown tool. Available tools: " + r.names() + "."
		return failed(call, ToolError(ErrToolUnknown, msg))
	}
	if call.BadInput {
		return failed(call, ToolError(ErrToolInput, "Arguments were not valid JSON. Send one JSON object matching the schema."))
	}
	out, err := safeCall(ctx, e.tool, call.Input)
	if err != nil {
		return failed(call, err)
	}
	return ToolResult{CallID: call.ID, Content: out.Content}, nil
}

func failed(call ToolCall, err error) (ToolResult, error) {
	return ToolResult{CallID: call.ID, Content: ModelMessage(err), IsError: true}, err
}

func (r *registry) names() string {
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// safeCall runs a tool on its own goroutine and turns a panic, or a tool
// that ends its goroutine with runtime.Goexit, into an error: one broken
// tool must not take the agent, and every other conversation in the
// process, down with it. The panic value goes to the host only.
func safeCall(ctx context.Context, tool Tool, input json.RawMessage) (ToolOutput, error) {
	type outcome struct {
		out ToolOutput
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		returned := false
		defer func() {
			if v := recover(); v != nil {
				done <- outcome{err: errors.New(ErrToolPanic, fmt.Sprintf("tool panicked: %v", v))}
			} else if !returned {
				done <- outcome{err: errors.New(ErrToolPanic, "tool exited without returning")}
			}
		}()
		out, err := tool.Call(ctx, bytes.Clone(input))
		returned = true
		done <- outcome{out, err}
	}()
	o := <-done
	return o.out, o.err
}
