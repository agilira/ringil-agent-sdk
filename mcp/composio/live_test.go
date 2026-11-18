// live_test.go: Composio Connect for real
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build live

package composio

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agilira/ringil-agent-sdk/mcp"
)

// TestLiveComposioSearch lists Composio's tools and runs its tool search,
// which reads Composio's catalog only: no app is touched.
func TestLiveComposioSearch(t *testing.T) {
	key := os.Getenv("COMPOSIO_PROJECT_KEY")
	if !strings.HasPrefix(key, "ck_") {
		t.Skip("no Composio consumer key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := mcp.Connect(ctx, Connect(func(context.Context) (string, error) { return key, nil }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Log(err)
		}
	}()
	for _, tool := range c.Tools() {
		if tool.Spec().Name != "composio__COMPOSIO_SEARCH_TOOLS" {
			continue
		}
		out, err := tool.Call(ctx, json.RawMessage(`{"queries":[{"use_case":"list Gmail messages"}],"session":{"generate_id":true}}`))
		if err != nil || !strings.Contains(out.Content, "GMAIL_") {
			t.Fatalf("search %.300q %v", out.Content, err)
		}
		return
	}
	t.Fatalf("no search tool among %d tools", len(c.Tools()))
}
