// server_test.go: our client talking to our server
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/session/file"
)

const serveToken = "a2a-test-token-0123456789abcdef012345"

// exposed serves a profile over A2A and returns its URL and the script.
func exposed(t *testing.T, steps ...fake.Step) (string, fake.Script) {
	t.Helper()
	script := fake.New(steps...)
	a, err := agent.New(agent.Config{Provider: script})
	if err != nil {
		t.Fatal(err)
	}
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := serve.New(serve.Config{Token: serveToken, Store: store, Profiles: []serve.Profile{{Name: "default", New: serve.Fixed(a)}}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h := httptest.NewServer(mux)
	t.Cleanup(func() { h.Close(); srv.Close() })
	exp, err := Expose(srv, "default", Card{Name: "Stock", Description: "Knows the stock.", Version: "1", URL: h.URL + Path,
		Skills: []Skill{{ID: "stock", Name: "stock", Description: "stock levels"}}})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/", exp)
	return h.URL, script
}

func bearer(token string) func(context.Context) (http.Header, error) {
	return func(context.Context) (http.Header, error) {
		return http.Header{"Authorization": {"Bearer " + token}}, nil
	}
}

func TestServeOverA2A(t *testing.T) {
	url, script := exposed(t, fake.Reply("314 units"), fake.Reply("still 314"))
	peer := connectTest(t, Remote{Name: "stock", URL: url, Poll: 20 * time.Millisecond, Headers: bearer(serveToken)})
	call := func(in askIn) string {
		raw, _ := json.Marshal(in)
		out, err := peer.Tool().Call(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		return out.Content
	}
	first := call(askIn{Message: "stock of AX-7?"})
	if !strings.HasPrefix(first, "314 units") || !strings.Contains(first, "state: completed") {
		t.Fatalf("%q", first)
	}
	// The same A2A context is the same conversation on the server.
	ctxID := regexp.MustCompile(`context_id: ([^,\]]+)`).FindStringSubmatch(first)[1]
	if second := call(askIn{Message: "and now?", ContextID: ctxID}); !strings.HasPrefix(second, "still 314") {
		t.Fatalf("%q", second)
	}
	if msgs := script.Requests()[1].Messages; len(msgs) != 3 {
		t.Fatalf("the second task saw %d messages", len(msgs))
	}
}

func TestA2ANeedsTheToken(t *testing.T) {
	url, _ := exposed(t, fake.Reply("x"))
	if _, err := Connect(context.Background(), Remote{Name: "stock", URL: url, Headers: bearer("wrong")}); err == nil {
		t.Fatal("connected without the token")
	}
}
