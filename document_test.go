// document_test.go: PDF documents beside images
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

var pdfBytes = []byte("%PDF-1.4\n" + strings.Repeat("x", 64*1024))

func TestCheckDocument(t *testing.T) {
	if _, err := Document(pdfBytes); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		mime string
		data []byte
	}{
		"empty":   {"application/pdf", nil},
		"not pdf": {"application/pdf", []byte("hello")},
		"label":   {"image/png", pdfBytes},
		"large":   {"application/pdf", append([]byte("%PDF-"), make([]byte, MaxDocumentBytes)...)},
	} {
		if CheckDocument(c.mime, c.data) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// A tool's PDF goes through the same check as its images, counts with
// them, costs by its size, and is noted when no longer shown.
func TestToolDocuments(t *testing.T) {
	doc, _ := Document(pdfBytes)
	tool := shotTool{ToolOutput{Content: "scan", Images: []Part{doc, {Type: PartDocument, MIME: "application/pdf", Data: []byte("not a pdf")}}}}
	reg, err := NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Execute(context.Background(), ToolCall{ID: "1", Name: "shot", Input: json.RawMessage(`{}`)})
	if err != nil || len(res.Images) != 1 || res.Images[0].Type != PartDocument {
		t.Fatalf("%d attachments, %v", len(res.Images), err)
	}
	msg := ToolMessage(res)
	if msg.Images() != 1 || attachmentTokens(msg) != documentTokens(pdfBytes) {
		t.Fatalf("counted %d, %d tokens", msg.Images(), attachmentTokens(msg))
	}
	noted := latestImages([]Message{msg, UserMessage("again", doc)}, 1)
	if noted[0].Images() != 0 || !strings.Contains(noted[0].Result.Content, "images or documents") {
		t.Fatalf("not noted: %q", noted[0].Result.Content)
	}
}

func TestRequestBytesWeightsDocuments(t *testing.T) {
	doc, err := Document(pdfBytes)
	if err != nil {
		t.Fatal(err)
	}
	with := requestBytes(&Request{Messages: []Message{UserMessage("read", doc)}})
	without := requestBytes(&Request{Messages: []Message{UserText("read")}})
	if with-without <= imageTokens*bytesPerToken {
		t.Fatalf("requestBytes did not weight PDF by size: diff=%d", with-without)
	}
}

// A PDF is counted by its pages when they are in plain sight: a hundred
// small pages are not a few thousand tokens.
func TestDocumentTokensByPages(t *testing.T) {
	data := []byte("%PDF-1.4\n" + strings.Repeat("<< /Type /Page /Parent 2 0 R >>\n", 100) + "<< /Type /Pages >>")
	if got := documentTokens(data); got != 100*documentPageTokens {
		t.Fatalf("%d", got)
	}
	// Incremental updates leave old pages behind: the tree's count wins.
	updated := []byte("%PDF-1.4\n" + strings.Repeat("<< /Type /Page >>\n", 30) + "<< /Type /Pages /Kids [] /Count 12 >>")
	if got := DocumentPages(updated); got != 12 {
		t.Fatalf("updated: %d", got)
	}
	// Pages in compressed object streams: unknown, and the cost guessed by
	// size.
	hidden := append([]byte("%PDF-1.5\n"), make([]byte, 640<<10)...)
	if DocumentPages(hidden) != 0 || documentTokens(hidden) != 10*documentPageTokens {
		t.Fatalf("hidden: %d pages, %d tokens", DocumentPages(hidden), documentTokens(hidden))
	}
}
