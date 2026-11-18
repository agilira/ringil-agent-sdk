// main.go: a small MCP server over stdio, for trying the MCP client
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Command notes serves the text files of one directory over MCP (stdio):
// list_notes and read_note, both read-only.
//
//	{"mcpServers": {"notes": {"command": "/path/to/notes", "args": ["/path/to/dir"]}}}
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type readIn struct {
	Name string `json:"name" jsonschema:"file name, as listed by list_notes"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: notes <dir>")
		os.Exit(2)
	}
	root, err := os.OpenRoot(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "notes", Version: "1"}, nil)
	readOnly := &sdk.ToolAnnotations{ReadOnlyHint: true}
	sdk.AddTool(s, &sdk.Tool{Name: "list_notes", Description: "List the notes", Annotations: readOnly},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			entries, err := os.ReadDir(os.Args[1])
			if err != nil {
				return nil, nil, err
			}
			var names []string
			for _, e := range entries {
				if e.Type().IsRegular() {
					names = append(names, e.Name())
				}
			}
			sort.Strings(names)
			return text(strings.Join(names, "\n")), nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "read_note", Description: "Read one note", Annotations: readOnly},
		func(_ context.Context, _ *sdk.CallToolRequest, in readIn) (*sdk.CallToolResult, any, error) {
			data, err := root.ReadFile(in.Name)
			if err != nil {
				return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "no such note"}}}, nil, nil
			}
			return text(string(data)), nil, nil
		})
	if err := s.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func text(s string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
}
