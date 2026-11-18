// tool.go: an MCP tool as an agent tool
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/agilira/go-errors"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	agent "github.com/agilira/ringil-agent-sdk"
)

type tool struct {
	c    *Client
	name string // the server's own name for it
	spec agent.ToolSpec
}

func (c *Client) adapt(t *sdk.Tool) (agent.Tool, bool) {
	schema, err := json.Marshal(t.InputSchema)
	if err != nil || len(schema) == 0 || schema[0] != '{' {
		return nil, false
	}
	return &tool{c: c, name: t.Name, spec: agent.ToolSpec{
		Name:        toolName(c.name, t.Name),
		Description: t.Description,
		InputSchema: schema,
		Annotations: annotations(t.Annotations),
	}}, true
}

// annotations follow the MCP defaults: a tool not marked read-only is
// destructive unless it says otherwise, and reaches the open world unless
// it says otherwise.
func annotations(a *sdk.ToolAnnotations) agent.Annotations {
	if a == nil {
		return agent.Annotations{Destructive: true, OpenWorld: true}
	}
	return agent.Annotations{
		ReadOnly:    a.ReadOnlyHint,
		Destructive: !a.ReadOnlyHint && (a.DestructiveHint == nil || *a.DestructiveHint),
		OpenWorld:   a.OpenWorldHint == nil || *a.OpenWorldHint,
	}
}

func (t *tool) Spec() agent.ToolSpec { return t.spec }

// Call runs the tool on the server. What the server returns is data for
// the model, never instructions to the host.
func (t *tool) Call(ctx context.Context, input json.RawMessage) (agent.ToolOutput, error) {
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		return agent.ToolOutput{}, agent.ToolError(agent.ErrToolInput, "Arguments must be a JSON object.")
	}
	res, err := t.c.session.CallTool(ctx, &sdk.CallToolParams{Name: t.name, Arguments: args})
	if err != nil {
		return agent.ToolOutput{}, errors.Wrap(err, ErrCall, "MCP call failed")
	}
	text := render(res)
	if res.IsError {
		return agent.ToolOutput{}, agent.ToolError(ErrCall, text)
	}
	return agent.ToolOutput{Content: text}, nil
}

// render turns a result into text: text content as it is, structured
// content as JSON, other content named.
func render(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if p := contentText(c); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(b))
		}
	}
	return strings.Join(parts, "\n")
}

func contentText(c sdk.Content) string {
	switch v := c.(type) {
	case *sdk.TextContent:
		return v.Text
	case *sdk.ImageContent:
		return "[image " + v.MIMEType + " not shown]"
	case *sdk.AudioContent:
		return "[audio " + v.MIMEType + " not shown]"
	case *sdk.ResourceLink:
		return "[resource " + v.URI + "]"
	case *sdk.EmbeddedResource:
		return embedded(v)
	}
	return ""
}

func embedded(r *sdk.EmbeddedResource) string {
	if r.Resource == nil {
		return "[resource]"
	}
	if r.Resource.Text != "" {
		return r.Resource.Text
	}
	return "[resource " + r.Resource.URI + " not shown]"
}
