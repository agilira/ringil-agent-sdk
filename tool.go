// tool.go: the tool contract and the errors a tool shows the model
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/agilira/go-errors"
)

// Tool is something the model can call. Most tools are built with
// [NewTool]; implement Tool directly for tools whose schema is not known at
// compile time (MCP servers, generated tools).
//
// Call receives the model's arguments as JSON. A returned error is shown to
// the model through [ModelMessage], which reveals only what the tool chose
// to reveal with [ToolError].
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, input json.RawMessage) (ToolOutput, error)
}

// ToolOutput is what a tool returns to the model.
type ToolOutput struct {
	Content string
}

// ToolOption refines a tool's spec.
type ToolOption func(*ToolSpec)

// ReadOnly marks a tool that changes nothing; the agent may run several at
// once.
func ReadOnly() ToolOption { return func(s *ToolSpec) { s.Annotations.ReadOnly = true } }

// Destructive marks a tool whose effects are hard to undo, so an approval
// hook can ask first.
func Destructive() ToolOption { return func(s *ToolSpec) { s.Annotations.Destructive = true } }

// OpenWorld marks a tool that reaches outside the host (network, email).
func OpenWorld() ToolOption { return func(s *ToolSpec) { s.Annotations.OpenWorld = true } }

// Tool error codes.
const (
	// ErrToolConfig: a tool or registry was built badly.
	ErrToolConfig errors.ErrorCode = "TOOL_CONFIG"
	// ErrToolInput: the model's arguments were refused.
	ErrToolInput errors.ErrorCode = "TOOL_INPUT"
	// ErrToolUnknown: the model called a tool that is not registered.
	ErrToolUnknown errors.ErrorCode = "TOOL_UNKNOWN"
	// ErrToolPanic: the tool panicked; the agent recovered.
	ErrToolPanic errors.ErrorCode = "TOOL_PANIC"
	// ErrToolFailed: the tool failed in a way it did not describe.
	ErrToolFailed errors.ErrorCode = "TOOL_FAILED"
)

// ToolError returns an error whose message the model may see. Use it for
// failures the model can act on ("file not found", "quota exceeded"); any
// other error reaches the model only as its code, because error text often
// carries paths, credentials or upstream responses.
func ToolError(code errors.ErrorCode, message string) error {
	return errors.New(code, message).WithUserMessage(message)
}

var publicCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// ModelMessage turns a tool error into the text the model sees: the
// message of the first [ToolError] in the chain, or only a code.
func ModelMessage(err error) string {
	code := ""
	for e := err; e != nil; e = unwrapOne(e) {
		coded, ok := e.(*errors.Error)
		if !ok {
			continue
		}
		if code == "" && publicCode.MatchString(string(coded.Code)) {
			code = string(coded.Code)
		}
		if coded.UserMsg != "" {
			return coded.UserMsg + " [" + string(coded.Code) + "]"
		}
	}
	if code != "" {
		return "Tool failed [" + code + "]."
	}
	return "Tool failed."
}

func unwrapOne(err error) error {
	if u, ok := err.(interface{ Unwrap() error }); ok {
		return u.Unwrap()
	}
	return nil
}
