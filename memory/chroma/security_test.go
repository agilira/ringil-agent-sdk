// security_test.go: what a Chroma server, or a memory, could do to us
//
// THREAT MODEL:
//   - A hostile or broken server: huge or malformed answers, redirects to
//     another origin (where the token would follow), errors that try to
//     reach the log or the model.
//   - Hostile text: a memory or a question built to break the word
//     search's regex, or to match what it should not.
//   - Leaks between people: one collection's memories in another's search.
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chroma

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
	"github.com/agilira/ringil-agent-sdk/memory/local"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Whatever the question, the word search is a valid regex that matches
// whole words of the question, never a pattern the question smuggled in.
func FuzzWordSearchPattern(f *testing.F) {
	for _, q := range []string{"acme", "a.*b", `\b(x|y)`, "(?s).*", "[", "café crème", "x{1000000}", "a|b|c", "\x00", "ﬁ ligature"} {
		f.Add(q)
	}
	f.Fuzz(func(t *testing.T, query string) {
		if len(query) > 2048 {
			return
		}
		for _, pattern := range wordRegexes(query) {
			checkWordPattern(t, query, pattern)
		}
	})
}

// checkWordPattern: the pattern compiles and matches no text that shares
// no word with the query.
func checkWordPattern(t *testing.T, query, pattern string) {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("query %q gave a bad regex: %v", query, err)
	}
	const text = "zzzz unrelated words qqqq"
	for _, w := range strings.Fields(text) {
		if slices.Contains(local.Terms(query), w) {
			return
		}
	}
	if re.MatchString(text) {
		t.Fatalf("query %q matches text it does not hold", query)
	}
}

// hostile is a server whose every answer is body, with status.
func hostile(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Whatever a server answers, the memory fails cleanly: no panic, an
// ErrMemoryIO or ErrMemoryInput.
func FuzzServerAnswers(f *testing.F) {
	for _, s := range []string{`{}`, `{"id":"x","configuration_json":{"hnsw":{"space":"cosine"}}}`, `[]`, `null`, `{"ids":[[]],"distances":[]}`,
		`{"id":"x","configuration_json":{"hnsw":null,"spann":null}}`, `{"ids":["a"],"documents":[null],"metadatas":[{"created":"x"}]}`, "\xff"} {
		f.Add(s, 200)
	}
	f.Add(`{"error":"X","message":"boom"}`, 500)
	f.Fuzz(func(t *testing.T, body string, status int) {
		if status < 200 || status > 599 || len(body) > 4096 {
			return
		}
		url := hostile(t, status, body)
		m, err := New(t.Context(), Config{URL: url, Collection: "abc", Embedder: &conceptEmbedder{model: "m"}})
		if err != nil {
			if !errors.HasCode(err, agent.ErrMemoryIO) && !errors.HasCode(err, agent.ErrMemoryInput) {
				t.Fatalf("New: %v", err)
			}
			return
		}
		_, _ = m.Search(t.Context(), "acme riunione", 3)
		_, _ = m.Search(t.Context(), "", 3)
		_, _ = m.Put(t.Context(), agent.MemoryRecord{Content: "fatto"})
		_ = m.Delete(t.Context(), "abc")
	})
}

func TestSecurityHugeAnswerIsBounded(t *testing.T) {
	url := hostile(t, 200, `{"id":"`+strings.Repeat("x", maxResponse+10)+`"}`)
	_, err := New(t.Context(), Config{URL: url, Collection: "abc", Embedder: &conceptEmbedder{model: "m"}})
	if !errors.HasCode(err, agent.ErrMemoryIO) {
		t.Fatalf("a huge answer: %v", err)
	}
}

// A redirect to another origin is not followed: the token stays home.
func TestSecurityNoRedirectToAnotherOrigin(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked = leaked || r.Header.Get("X-Chroma-Token") != ""
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	headers := func(context.Context) (http.Header, error) { return http.Header{"X-Chroma-Token": {"s3cret"}}, nil }
	if _, err := New(t.Context(), Config{URL: srv.URL, Collection: "abc", Embedder: &conceptEmbedder{model: "m"}, Headers: headers}); err == nil {
		t.Fatal("a redirect answer was taken as the collection")
	}
	if leaked {
		t.Fatal("the token followed a redirect to another origin")
	}
}

// A server's error message reaches the caller clipped, and the token in
// the request never does.
func TestSecurityErrorsAreClippedAndHoldNoToken(t *testing.T) {
	url := hostile(t, 500, `{"error":"X","message":"`+strings.Repeat("m", 5000)+`"}`)
	headers := func(context.Context) (http.Header, error) { return http.Header{"X-Chroma-Token": {"s3cret"}}, nil }
	_, err := New(t.Context(), Config{URL: url, Collection: "abc", Embedder: &conceptEmbedder{model: "m"}, Headers: headers})
	if err == nil || len(err.Error()) > 600 || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error %d bytes: %.120q", len(err.Error()), err)
	}
}

// A collection that measures by another distance would make the minimum
// similarity meaningless: refused.
func TestSecurityOnlyCosineCollections(t *testing.T) {
	f, srv := newFake(t)
	f.mu.Lock()
	c := &fakeCollection{id: fakeUUID(1), name: "lone", space: "l2", recs: map[string]fakeRecord{}}
	f.byID[c.id], f.names["lone"] = c, c.id
	f.mu.Unlock()
	if _, err := New(t.Context(), Config{URL: srv.URL, Collection: "lone", Embedder: &conceptEmbedder{model: "m"}}); !errors.HasCode(err, agent.ErrMemoryInput) {
		t.Fatalf("an l2 collection: %v", err)
	}
}

// Hostile memory text is stored and found as text: a regex in a memory,
// or in a question, matches only as the words it spells.
func TestSecurityRegexInTextIsJustText(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, collectionName(t, url, "rx"), &conceptEmbedder{model: "concepts"})
	put(t, m, "la regola è (.*) e basta")
	put(t, m, "nota qualunque")
	s := m.(*store)
	if len(wordRegexes(".* (.*) [a-z]+")) != 0 {
		t.Fatal("a question with no words made a pattern")
	}
	got, err := s.byWords(t.Context(), "regola.* qualunque|nota", 5)
	if err != nil || len(got) != 2 {
		t.Fatalf("the words of a question with metacharacters: %q %v", contents(got), err)
	}
	got, _ = s.byWords(t.Context(), "basta(.*)", 5)
	if len(got) != 1 || !strings.Contains(got[0].Content, "regola") {
		t.Fatalf("one word: %q", contents(got))
	}
}
