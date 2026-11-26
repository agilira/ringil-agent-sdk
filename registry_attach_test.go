// registry_attach_test.go: a tool's attachments, bounded
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"testing"
)

func TestToolAttachmentsBounded(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 100)...)
	var many []Part
	for range 50 {
		many = append(many, Part{Type: PartImage, MIME: "image/png", Data: png})
	}
	if got := checkedImages(many); len(got) != maxToolAttachments {
		t.Fatalf("%d kept", len(got))
	}
	big := Part{Type: PartDocument, MIME: "application/pdf", Data: append([]byte("%PDF-1.4"), bytes.Repeat([]byte(" "), 15<<20)...)}
	if got := checkedImages([]Part{big, big}); len(got) != 1 {
		t.Fatalf("%d large PDFs kept", len(got))
	}
}
