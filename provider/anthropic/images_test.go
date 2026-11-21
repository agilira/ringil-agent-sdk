// images_test.go: a request never carries more images than the API takes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package anthropic

import (
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
)

func screenshots(n int) []agent.Message {
	img := agent.Part{Type: agent.PartImage, MIME: "image/png", Data: []byte("png")}
	var msgs []agent.Message
	for i := range n {
		id := "c" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		msgs = append(msgs, agent.Message{Role: agent.RoleAssistant, Calls: []agent.ToolCall{{ID: id, Name: "shot", Input: []byte(`{}`)}}},
			agent.ToolMessage(agent.ToolResult{CallID: id, Content: "shot", Images: []agent.Part{img}}))
	}
	return msgs
}

func checkWindow(t *testing.T, n int, got []agent.Message) {
	t.Helper()
	k := images(got)
	if n <= maxImages {
		if k != n {
			t.Errorf("%d screenshots: %d sent", n, k)
		}
		return
	}
	if k > maxImages || k <= maxImages-imageStep {
		t.Errorf("%d screenshots: %d sent", n, k)
	}
	if got[len(got)-1].Images() != 1 || !strings.Contains(got[1].Result.Content, "no longer shown") {
		t.Errorf("%d: the newest went, or the oldest has no note", n)
	}
}

func images(msgs []agent.Message) int {
	n := 0
	for _, m := range msgs {
		n += m.Images()
	}
	return n
}

// At most maxImages go, the newest; the older leave in steps, so the
// turns before stay the same from one request to the next (audit 21/11:
// past 100 screenshots, or 20 large ones, every request was refused).
func TestImagesWithinTheAPILimit(t *testing.T) {
	for _, n := range []int{5, 20, 21, 29, 30, 31, 150} {
		checkWindow(t, n, fitImages(screenshots(n)))
	}
	// One more screenshot within the same step leaves the earlier
	// messages as they were.
	a, b := fitImages(screenshots(22)), fitImages(screenshots(23))
	for i := range a {
		if a[i].Images() != b[i].Images() {
			t.Fatalf("message %d changed between 22 and 23 screenshots", i)
		}
	}
}
