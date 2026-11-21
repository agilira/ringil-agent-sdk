// live_test.go: Mem0's own model at work, against a real server
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mem0

import (
	"os"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// What the server's model does with what is remembered: a text with two
// facts keeps them; the same text again, a greeting, a question add none,
// and Put says so. It needs RINGIL_MEM0_URL and RINGIL_MEM0_INFER=1 (each Put is a
// call to the server's model).
func TestLiveExtraction(t *testing.T) {
	url := os.Getenv("RINGIL_MEM0_URL")
	if url == "" || os.Getenv("RINGIL_MEM0_INFER") != "1" {
		t.Skip("needs RINGIL_MEM0_URL and RINGIL_MEM0_INFER=1")
	}
	m := open(t, url, user(t, "a"), false)
	first := put(t, m, "Il magazzino di Torino apre alle otto e il mio cliente principale è Acme.")
	all, _ := m.Search(t.Context(), "", 10)
	t.Logf("kept %q, first %q", contents(all), first.Content)
	if len(all) < 2 {
		t.Fatalf("two facts, kept %q", contents(all))
	}
	for _, text := range []string{"Il magazzino di Torino apre alle otto e il mio cliente principale è Acme.", "ciao!", "A che ora apre il magazzino di Torino?"} {
		got, err := m.Put(t.Context(), agent.MemoryRecord{Content: text})
		if !errors.HasCode(err, agent.ErrMemoryInput) {
			t.Errorf("%q, nothing new: %+v %v", text, got, err)
		}
	}
	if after, _ := m.Search(t.Context(), "", 10); len(after) != len(all) {
		t.Fatalf("kept more: %q", contents(after))
	}
	if got, _ := m.Search(t.Context(), "a che ora apre il magazzino?", 3); len(got) == 0 {
		t.Fatal("the opening hour is not found")
	}
}
