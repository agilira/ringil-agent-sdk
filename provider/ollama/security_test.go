// security_test.go: what an Ollama server could do to us
//
// THREAT MODEL:
//   - A hostile or broken server (a proxy in front of Ollama): huge or
//     malformed replies, vectors that do not match the texts, redirects to
//     another origin where the headers would follow, error messages built
//     to flood the log.
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

func replying(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Whatever the reply, Embed returns vectors that match the texts, or an
// error: never a panic, never a short or ragged answer.
func FuzzReplies(f *testing.F) {
	for _, s := range []string{`{"embeddings":[[1,2],[3,4]]}`, `{"embeddings":[[1],[2,3]]}`, `{"embeddings":[[]]}`, `{"embeddings":null}`,
		`{"embeddings":[[1,2]]} {}`, `{"error":"x"}`, `[]`, "\xff", `{"embeddings":[[1e40,2]]}`} {
		f.Add(s, 200)
	}
	f.Add(`{"error":"boom"}`, 500)
	f.Fuzz(func(t *testing.T, body string, status int) {
		if status < 200 || status > 599 || len(body) > 8192 {
			return
		}
		e, err := NewEmbedder(Config{URL: replying(t, status, body), Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		vecs, err := e.Embed(t.Context(), []string{"a", "b"})
		if err == nil {
			checkVectors(t, body, vecs)
		}
	})
}

func checkVectors(t *testing.T, body string, vecs [][]float32) {
	t.Helper()
	if len(vecs) != 2 || len(vecs[0]) == 0 || len(vecs[0]) != len(vecs[1]) {
		t.Fatalf("accepted %q: %v", body, vecs)
	}
}

func TestSecurityHugeReplyIsBounded(t *testing.T) {
	e, _ := NewEmbedder(Config{URL: replying(t, 200, `{"embeddings":[[`+strings.Repeat("1,", maxReply/2)+`1]]}`), Model: "m"})
	if _, err := e.Embed(t.Context(), []string{"a"}); err == nil {
		t.Fatal("a reply past the bound was read")
	}
}

func TestSecurityNoRedirectToAnotherOrigin(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = leaked || r.Header.Get("Authorization") != ""
		_, _ = w.Write([]byte(`{"embeddings":[[1,2]]}`))
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	headers := func(context.Context) (http.Header, error) {
		return http.Header{"Authorization": {"Bearer s3cret"}}, nil
	}
	e, _ := NewEmbedder(Config{URL: srv.URL, Model: "m", Headers: headers})
	if _, err := e.Embed(t.Context(), []string{"a"}); err == nil {
		t.Fatal("a redirect was followed to another origin")
	}
	if leaked {
		t.Fatal("the headers followed a redirect to another origin")
	}
}

func TestSecurityErrorsAreClipped(t *testing.T) {
	e, _ := NewEmbedder(Config{URL: replying(t, 400, `{"error":"`+strings.Repeat("e", 10000)+`"}`), Model: "m"})
	_, err := e.Embed(t.Context(), []string{"a"})
	if !errors.HasCode(err, agent.ErrEmbed) || len(err.Error()) > 500 {
		t.Fatalf("%d bytes: %.80q", len(err.Error()), err)
	}
}
