// tool.go: an MCP tool as an agent tool
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agilira/go-errors"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	agent "github.com/agilira/ringil-agent-sdk"
)

type tool struct {
	c    *Client
	name string // the server's own name for it
	spec agent.ToolSpec
}

func (c *Client) adapt(t *sdk.Tool, taken map[string]bool) (agent.Tool, bool) {
	schema, ok := objectSchema(t.InputSchema)
	if !ok {
		return nil, false
	}
	ann := agent.Annotations{Destructive: true, OpenWorld: true}
	if c.srv.TrustAnnotations {
		ann = annotations(t.Annotations)
	}
	return &tool{c: c, name: t.Name, spec: agent.ToolSpec{
		Name:        toolName(c.name, t.Name, taken),
		Description: clip(t.Description, 4096),
		InputSchema: schema,
		Annotations: ann,
	}}, true
}

// objectSchema accepts an input schema only if it describes an object
// and is of sane size: one malformed tool must not make the provider
// refuse every request.
func objectSchema(s any) (json.RawMessage, bool) {
	raw, err := json.Marshal(s)
	if err != nil || len(raw) == 0 || len(raw) > maxSchema {
		return nil, false
	}
	var top struct {
		Type any `json:"type"`
	}
	if err := json.Unmarshal(raw, &top); err != nil || raw[0] != '{' {
		return nil, false
	}
	return raw, top.Type == nil || top.Type == "object"
}

// annotations follow the MCP defaults: a tool not marked read-only is
// destructive unless it says otherwise, and reaches the open world unless
// it says otherwise. They are only hints from the server: unless the
// host trusts the server (TrustAnnotations), every tool counts as
// destructive, so an approval hook asks before each call.
func annotations(a *sdk.ToolAnnotations) agent.Annotations {
	if a == nil {
		return agent.Annotations{Destructive: true, OpenWorld: true}
	}
	openWorld := a.OpenWorldHint == nil || *a.OpenWorldHint
	if a.ReadOnlyHint {
		// destructiveHint matters only for tools that write; a tool
		// claiming both is taken as destructive.
		contradicts := a.DestructiveHint != nil && *a.DestructiveHint
		return agent.Annotations{ReadOnly: !contradicts, Destructive: contradicts, OpenWorld: openWorld}
	}
	return agent.Annotations{Destructive: a.DestructiveHint == nil || *a.DestructiveHint, OpenWorld: openWorld}
}

func (t *tool) Spec() agent.ToolSpec { return t.spec }

// Call runs the tool on the server, bounded in time and in what it may
// return. What the server returns is data for the model, never
// instructions to the host.
func (t *tool) Call(ctx context.Context, input json.RawMessage) (agent.ToolOutput, error) {
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		return agent.ToolOutput{}, agent.ToolError(agent.ErrToolInput, "Arguments must be a JSON object.")
	}
	session := t.c.current()
	if session == nil || t.c.life.Err() != nil {
		return agent.ToolOutput{}, agent.ToolError(ErrCall, "The MCP server is closed.")
	}
	ctx, cancel := context.WithTimeout(ctx, t.c.srv.callTimeout())
	defer cancel()
	stop := context.AfterFunc(t.c.life, cancel)
	defer stop()
	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: t.name, Arguments: args})
	if err != nil {
		return agent.ToolOutput{}, errors.Wrap(err, ErrCall, "MCP call failed")
	}
	text := capText(render(res), t.c.srv.maxOutput())
	if res.IsError {
		return agent.ToolOutput{}, agent.ToolError(ErrCall, text)
	}
	return agent.ToolOutput{Content: text}, nil
}

func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[" + strconv.Itoa(len(s)-cut) + " more bytes of the result not shown]"
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
