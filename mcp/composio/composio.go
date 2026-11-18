// composio.go: Composio as an MCP server
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package composio connects an agent to Composio, which reaches hundreds
// of business apps (Gmail, Slack, GitHub, Notion...) through one MCP
// server. Connect is Composio Connect: a consumer key (ck_...) of the
// user, whose own connected accounts the tools act on.
//
// Composio offers meta-tools: COMPOSIO_SEARCH_TOOLS finds app tools,
// COMPOSIO_MULTI_EXECUTE_TOOL runs them, COMPOSIO_MANAGE_CONNECTIONS
// links new accounts, plus a remote workbench and bash. All of them are
// offered; Server.Tools narrows the list when a host wants less, and the
// host's approval hook sees every call, since the server's hints are not
// trusted by default.
package composio

import (
	"context"
	"net/http"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/mcp"
)

// ConnectURL is Composio Connect's MCP endpoint.
const ConnectURL = "https://connect.composio.dev/mcp"

// Key returns the consumer key, read when needed (from the host's
// secrets) so that it is never stored in the server description.
type Key func(ctx context.Context) (string, error)

// Connect returns the description of Composio Connect for mcp.Connect,
// named "composio"; set Tools or other fields on it before connecting.
func Connect(key Key) mcp.Server {
	if key == nil {
		key = func(context.Context) (string, error) { return "", nil }
	}
	return mcp.Server{
		Name: "composio",
		URL:  ConnectURL,
		Headers: func(ctx context.Context) (http.Header, error) {
			k, err := key(ctx)
			if err != nil {
				return nil, err
			}
			if k == "" {
				return nil, errors.New(mcp.ErrConfig, "the Composio key is empty")
			}
			return http.Header{"X-Consumer-Api-Key": {k}}, nil
		},
	}
}
