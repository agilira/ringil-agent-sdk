// tool_func_test.go: schema error messages
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
)

// A schema error says which fields to send: small models give up otherwise.
func TestInvalidArgumentsNameTheFields(t *testing.T) {
	type in struct {
		Task string `json:"task" desc:"The complete task"`
		Note string `json:"note,omitempty"`
	}
	tool := agent.NewTool("warehouse", "w", func(context.Context, in) (string, error) { return "", nil })
	_, err := tool.Call(context.Background(), json.RawMessage(`{"object":"AX-7"}`))
	if err == nil || !strings.Contains(err.Error(), "Call warehouse again with: note (optional); task (required: The complete task).") {
		t.Fatalf("%v", err)
	}
}
