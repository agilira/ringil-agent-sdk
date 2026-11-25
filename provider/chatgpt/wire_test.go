// wire_test.go: what goes to the subscription's backend
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
)

// The subscription's backend takes no files: a PDF is named, never sent
// as an image.
func TestDocumentIsNamed(t *testing.T) {
	out := string(toolOutput(agent.ToolResult{Content: "text", Images: []agent.Part{{Type: agent.PartDocument, MIME: "application/pdf", Data: []byte("%PDF-1")}}}))
	if strings.Contains(out, "input_image") || strings.Contains(out, "JVBER") || !strings.Contains(out, "PDF documents here") {
		t.Fatalf("%s", out)
	}
}
