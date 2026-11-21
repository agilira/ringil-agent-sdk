// security_test.go: what a Mem0 server, or a memory, could do to us
//
// THREAT MODEL:
//   - The server's own reach: it reads, changes and deletes any ID it is
//     given, whoever's it is. An ID from another memory (seen in a shared
//     chat, guessed, planted in a document) must stay out of reach.
//   - A hostile or broken server: huge or malformed answers, records of
//     other users in a search, redirects to another origin (where the key
//     would follow), errors that try to reach the log or the model.
//   - Hostile text: a memory or a question built to reach another user, or
//     a path on the server, through the request.
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mem0

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

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
// ErrMemoryIO, ErrMemoryInput or ErrMemoryNotFound, and never a record of
// another user.
func FuzzServerAnswers(f *testing.F) {
	for _, s := range []string{`{}`, `null`, `[]`, `{"results":null}`, `{"results":[{}]}`,
		`{"results":[{"id":"00000000-0000-4000-8000-000000000001","memory":"x","user_id":"other"}]}`,
		`{"results":[{"id":"../reset","memory":"x","user_id":"a","event":"ADD"}]}`,
		`{"results":[{"id":"00000000-0000-4000-8000-000000000001","memory":"","user_id":"a","created_at":"x"}]}`, "\xff"} {
		f.Add(s, 200)
	}
	f.Add(`{"detail":[{"loc":["body"],"msg":"bad"}]}`, 422)
	f.Add(`{"detail":"boom"}`, 502)
	f.Fuzz(func(t *testing.T, body string, status int) {
		if status < 200 || status > 599 || len(body) > 4096 {
			return
		}
		url := hostile(t, status, body)
		m, err := New(t.Context(), Config{URL: url, User: "a"})
		if err != nil {
			if !errors.HasCode(err, agent.ErrMemoryIO) && !errors.HasCode(err, agent.ErrMemoryInput) {
				t.Fatalf("New: %v", err)
			}
			return
		}
		exercise(t, m)
	})
}

// exercise makes every call; what comes back must be a record of ours.
func exercise(t *testing.T, m agent.Memory) {
	t.Helper()
	for _, q := range []string{"acme", ""} {
		recs, _ := m.Search(t.Context(), q, 3)
		for _, r := range recs {
			if !validID.MatchString(r.ID) || r.Content == "" {
				t.Fatalf("a record the server should not have given: %+v", r)
			}
		}
	}
	_, _ = m.Put(t.Context(), agent.MemoryRecord{Content: "fatto"})
	_ = m.Delete(t.Context(), "00000000-0000-4000-8000-000000000001")
}

// Whatever is remembered and asked, against the fake: the memory answers
// or refuses the input, never an I/O error, and the record is this user's.
func FuzzPutAndSearch(f *testing.F) {
	for _, s := range []string{"a\" OR \"b", "../../reset", "?user_id=other", "{\"user_id\":\"other\"}", "\x00", "é\u202e", "ciao"} {
		f.Add(s, s)
	}
	fake := &fakeMem0{recs: map[string]fakeRecord{}}
	srv := httptest.NewServer(http.HandlerFunc(fake.serve))
	f.Cleanup(srv.Close)
	m, err := New(context.Background(), Config{URL: srv.URL, User: "a"})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, content, query string) {
		if len(content) > 4096 || len(query) > 4096 {
			return
		}
		putAndSearch(t, m, content, query)
		if other := fake.othersThan("a"); other != "" {
			t.Fatalf("a record kept for %q", other)
		}
	})
}

// putAndSearch: the memory answers or refuses the input, never an I/O
// error (past 1000 records an empty search is partial).
func putAndSearch(t *testing.T, m agent.Memory, content, query string) {
	t.Helper()
	if _, err := m.Put(t.Context(), agent.MemoryRecord{Content: content}); err != nil && !errors.HasCode(err, agent.ErrMemoryInput) {
		t.Fatalf("Put %q: %v", content, err)
	}
	if _, err := m.Search(t.Context(), query, 5); err != nil && !errors.HasCode(err, agent.ErrMemoryPartial) {
		t.Fatalf("Search %q: %v", query, err)
	}
}

// An ID never becomes a path or a query: one that could is refused before
// any call, and one of the caller's travels only as a value.
func TestSecurityIDsNeverBecomePaths(t *testing.T) {
	f, srv := newFake(t)
	m, err := New(t.Context(), Config{URL: srv.URL, User: "a"})
	if err != nil {
		t.Fatal(err)
	}
	before := f.calls
	for _, id := range []string{"../reset", "%2e%2e/reset", "x?user_id=b", "00000000-0000-4000-8000-000000000001/history", "a b"} {
		if err := m.Delete(t.Context(), id); !errors.HasCode(err, agent.ErrMemoryNotFound) {
			t.Errorf("Delete %q: %v", id, err)
		}
		if _, err := m.Put(t.Context(), agent.MemoryRecord{ID: id, Content: "x"}); !errors.HasCode(err, agent.ErrMemoryInput) {
			t.Errorf("Put %q: %v", id, err)
		}
	}
	if f.calls != before {
		t.Fatalf("%d calls reached the server", f.calls-before)
	}
}

// An ID of the caller's travels as a value (JSON, or one query value),
// never in a path.
func TestSecurityCallersIDsStayInJSON(t *testing.T) {
	f, srv := newFake(t)
	m, err := New(t.Context(), Config{URL: srv.URL, User: "a"})
	if err != nil {
		t.Fatal(err)
	}
	const named = "reset-memories_00000000000G"
	if _, err := m.Put(t.Context(), agent.MemoryRecord{ID: named, Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(t.Context(), named); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.paths {
		onlyAsValue(t, p, named)
	}
}

// onlyAsValue: the request p holds id at most as its one run_id value.
func onlyAsValue(t *testing.T, p, id string) {
	t.Helper()
	u, err := url.Parse(p)
	if err != nil || strings.Contains(u.Path, id) {
		t.Fatalf("the caller's ID in a path: %s", p)
	}
	if run := u.Query()["run_id"]; len(run) > 0 && (len(run) != 1 || run[0] != id) {
		t.Fatalf("the caller's ID as a query: %s", p)
	}
}

// A server whose record says it is kept under another ID of ours, or of
// another user: the record answers to neither.
func TestSecurityPlantedIDs(t *testing.T) {
	body := `{"results":[{"id":"00000000-0000-4000-8000-000000000001","memory":"di b","user_id":"b","run_id":"mine"},` +
		`{"id":"00000000-0000-4000-8000-000000000002","memory":"male","user_id":"a","run_id":"../x"}]}`
	m, err := New(t.Context(), Config{URL: hostile(t, 200, body), User: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Search(t.Context(), "", 5); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// A server that answers a search with other users' records: they are left
// out, whatever its filter did.
func TestSecurityOtherUsersRecordsAreLeftOut(t *testing.T) {
	body := `{"results":[` +
		`{"id":"00000000-0000-4000-8000-000000000001","memory":"di b","user_id":"b","score":0.9},` +
		`{"id":"00000000-0000-4000-8000-000000000002","memory":"di nessuno","score":0.9},` +
		`{"id":"00000000-0000-4000-8000-000000000003","memory":"di a","user_id":"a","score":0.8}]}`
	m, err := New(t.Context(), Config{URL: hostile(t, 200, body), User: "a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"x", ""} {
		got, err := m.Search(t.Context(), q, 5)
		if err != nil || len(got) != 1 || got[0].Content != "di a" {
			t.Fatalf("%q: %q %v", q, contents(got), err)
		}
	}
}

func TestSecurityHugeAnswerIsBounded(t *testing.T) {
	url := hostile(t, 200, `{"results":[{"id":"`+strings.Repeat("x", maxResponse+10)+`"}]}`)
	if _, err := New(t.Context(), Config{URL: url, User: "a"}); !errors.HasCode(err, agent.ErrMemoryIO) {
		t.Fatalf("a huge answer: %v", err)
	}
}

// A redirect to another origin is not followed: the key stays home.
func TestSecurityNoRedirectToAnotherOrigin(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked = leaked || r.Header.Get("X-API-Key") != ""
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	headers := func(context.Context) (http.Header, error) { return http.Header{"X-Api-Key": {"s3cret"}}, nil }
	if _, err := New(t.Context(), Config{URL: srv.URL, User: "a", Headers: headers}); err == nil {
		t.Fatal("a redirect was taken as the answer")
	}
	if leaked {
		t.Fatal("the key followed a redirect to another origin")
	}
}

// A server's error reaches the caller clipped, and the key never does.
func TestSecurityErrorsAreClippedAndHoldNoKey(t *testing.T) {
	url := hostile(t, 500, `{"detail":"`+strings.Repeat("m", 5000)+`"}`)
	headers := func(context.Context) (http.Header, error) { return http.Header{"X-Api-Key": {"s3cret"}}, nil }
	_, err := New(t.Context(), Config{URL: url, User: "a", Headers: headers})
	if err == nil || len(err.Error()) > 600 || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error %d bytes: %.120q", len(err.Error()), err)
	}
}

// The user goes as a query value and a JSON string, never as syntax: a
// user that would smuggle another is refused before any call.
func TestSecurityUserIsNeverSyntax(t *testing.T) {
	for _, u := range []string{"a&user_id=b", "a\nb", "a\"}", "../a", "", "a b"} {
		if _, err := New(t.Context(), Config{URL: "http://127.0.0.1:1", User: u}); !errors.HasCode(err, agent.ErrMemoryInput) {
			t.Errorf("%q: %v", u, err)
		}
	}
}
