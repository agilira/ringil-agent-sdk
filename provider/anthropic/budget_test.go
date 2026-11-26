// budget_test.go: attachments that fit a request together
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package anthropic

import (
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
)

func pdfPart(size, pages int) agent.Part {
	body := "%PDF-1.4\n" + strings.Repeat("<< /Type /Page >>\n", pages)
	if pad := size - len(body); pad > 0 {
		body += strings.Repeat(" ", pad)
	}
	return agent.Part{Type: agent.PartDocument, MIME: "application/pdf", Data: []byte(body)}
}

func withPart(p agent.Part) agent.Message {
	return agent.Message{Role: agent.RoleUser, Parts: []agent.Part{{Type: agent.PartText, Text: "see"}, p}}
}

// Two PDFs too large together, or of too many pages together: the oldest
// goes, the latest stays.
func TestFitAttachmentsBudget(t *testing.T) {
	for name, parts := range map[string][2]agent.Part{
		"bytes": {pdfPart(15<<20, 1), pdfPart(15<<20, 1)},
		"pages": {pdfPart(1000, 60), pdfPart(1000, 60)},
	} {
		got := fitImages([]agent.Message{withPart(parts[0]), withPart(parts[1])})
		if got[0].Images() != 0 || got[1].Images() != 1 {
			t.Errorf("%s: kept %d and %d", name, got[0].Images(), got[1].Images())
		}
	}
	small := []agent.Message{withPart(pdfPart(1000, 10)), withPart(pdfPart(1000, 10))}
	if got := fitImages(small); got[0].Images() != 1 || got[1].Images() != 1 {
		t.Error("small PDFs trimmed")
	}
}
