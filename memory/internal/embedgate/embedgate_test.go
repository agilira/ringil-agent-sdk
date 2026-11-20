// embedgate_test.go: the pause after a failure
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package embedgate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

type stub struct {
	calls, queries int
	fail, short    bool
	got            []string
}

func (s *stub) Model() string { return "m" }

func (s *stub) Embed(_ context.Context, texts []string) ([][]float32, error) {
	s.calls++
	s.got = texts
	if s.fail {
		return nil, errors.New(agent.ErrEmbed, "down")
	}
	n := len(texts)
	if s.short {
		n--
	}
	return make([][]float32, n), nil
}

func (s *stub) EmbedQuery(ctx context.Context, q []string) ([][]float32, error) {
	s.queries++
	return s.Embed(ctx, q)
}

func TestAFailureRestsTheEmbedder(t *testing.T) {
	s := &stub{fail: true}
	g := New(s)
	now := time.Unix(1000, 0)
	g.now = func() time.Time { return now }
	if _, ok := g.Embed(t.Context(), []string{"a"}, false); ok || s.calls != 1 {
		t.Fatal("a failure passed")
	}
	if _, ok := g.Embed(t.Context(), []string{"a"}, false); ok || s.calls != 1 {
		t.Fatal("called during the pause")
	}
	now = now.Add(Pause)
	s.fail = false
	if _, ok := g.Embed(t.Context(), []string{"a"}, false); !ok || s.calls != 2 {
		t.Fatal("not called after the pause")
	}
}

func TestShortAnswersAndQueries(t *testing.T) {
	s := &stub{short: true}
	g := New(s)
	if _, ok := g.Embed(t.Context(), []string{"a", "b"}, false); ok {
		t.Fatal("an answer short of vectors passed")
	}
	g.Wake()
	s.short = false
	if _, ok := g.Embed(t.Context(), []string{"q"}, true); !ok || s.queries != 1 {
		t.Fatal("a query did not go through EmbedQuery")
	}
}

func TestLongTextsAreClippedOnACharacter(t *testing.T) {
	s := &stub{}
	g := New(s)
	long := strings.Repeat("é", MaxChars) // two bytes each
	if _, ok := g.Embed(t.Context(), []string{long}, false); !ok {
		t.Fatal("not embedded")
	}
	if got := s.got[0]; len(got) > MaxChars || !strings.HasSuffix(got, "é") {
		t.Fatalf("clipped to %d bytes, ends %q", len(got), got[len(got)-2:])
	}
}

// A cancelled call is not the embedder's fault: no pause.
func TestACancelDoesNotRest(t *testing.T) {
	s := &stub{fail: true}
	g := New(s)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	g.Embed(ctx, []string{"a"}, false)
	s.fail = false
	if _, ok := g.Embed(t.Context(), []string{"a"}, false); !ok {
		t.Fatal("a cancel started a pause")
	}
}
