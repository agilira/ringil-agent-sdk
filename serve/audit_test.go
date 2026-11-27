// audit_test.go: what the audit of 26/11 found in the API for an app
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// A cookie of the name set by a page on another port, sent first, does
// not sign the app out.
func TestAppCookieTossed(t *testing.T) {
	a := appAPI(t)
	cookie := a.signedIn()
	resp := a.raw("GET", "/v1/profiles", "Cookie", "ringil_session=planted; "+cookie, "Sec-Fetch-Site", "same-origin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body(resp))
	}
}

// Reads and titles share a conversation, a delete has it alone, and no
// run starts while any of them is going.
func TestConversationHeld(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ok"))})}})
	s := a.srv
	admit := func() error { s.mu.Lock(); defer s.mu.Unlock(); return s.admit("chat1") }
	held := func(id string, kind holdKind) func() {
		t.Helper()
		release, err := s.hold(id, kind)
		if err != nil {
			t.Fatalf("%s %d: %v", id, kind, err)
		}
		return release
	}
	r1, r2, r3 := held("Chat1", holdRead), held("chat1", holdRead), held("chat1", holdLabel) // two tabs and a rename
	if !errors.HasCode(admit(), ErrBusy) {
		t.Fatal("a run during a read")
	}
	if _, err := s.hold("chat1", holdExclude); !errors.HasCode(err, ErrBusy) {
		t.Fatalf("a delete during a read: %v", err)
	}
	r1()
	r2()
	r3()
	del := held("chat1", holdExclude)
	if _, err := s.hold("chat1", holdRead); !errors.HasCode(err, ErrBusy) {
		t.Fatalf("a read during a delete: %v", err)
	}
	del()
	if err := admit(); err != nil || len(s.held) != 0 {
		t.Fatalf("after release: %v %v", err, s.held)
	}
}

// Without a store that keeps labels, renaming is "not found", not a
// server error.
func TestRenameWithoutLabels(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	title := "x"
	if err := a.srv.ChangeConversation(context.Background(), "abc", ConversationChange{Title: &title}); status(err) != http.StatusNotFound {
		t.Fatalf("%v → %d", err, status(err))
	}
}

// A voice model that cannot be set up is a server error, not "no model".
func TestVoiceSetupFails(t *testing.T) {
	broken := func(context.Context) (agent.Transcriber, error) { return nil, errors.New("X", "no key") }
	a := voiceAPI(t, Voice{Transcriber: broken})
	if code, _, out := a.send("POST", "/v1/audio/transcriptions", "audio/ogg", "x", true); code != http.StatusInternalServerError {
		t.Fatalf("%d %s", code, out)
	}
}

// Large bodies are read MaxRuns at once.
func TestUploadSlots(t *testing.T) {
	a := newAPI(t, Config{MaxRuns: 1, Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	a.srv.uploads <- struct{}{} // one held
	big := `{"profile":"default","input":"x","attachments":[{"mime":"image/png","data":"` + strings.Repeat("A", maxBody) + `"}]}`
	if code, _, out := a.send("POST", "/v1/runs", "application/json", big, true); code != http.StatusTooManyRequests {
		t.Fatalf("%d %.200s", code, out)
	}
	if code, _, out := a.send("POST", "/v1/runs", "application/json", `{"profile":"default","input":"x"}`, true); code != http.StatusCreated {
		t.Fatalf("a plain message waits for no slot: %d %s", code, out)
	}
	<-a.srv.uploads
}

// Paused, no run starts, and the pause waits for the runs going.
func TestPause(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ok"))})}})
	resume, err := a.srv.Pause(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if code, out := a.do("POST", "/v1/runs", `{"profile":"default","input":"x"}`); code != http.StatusConflict && code != http.StatusTooManyRequests && code != http.StatusServiceUnavailable {
		t.Fatalf("a run while paused: %d %s", code, out)
	}
	resume()
	if code, out := a.do("POST", "/v1/runs", `{"profile":"default","input":"x"}`); code != http.StatusCreated {
		t.Fatalf("after resume: %d %s", code, out)
	}
	a.srv.mu.Lock()
	a.srv.active++ // a run that does not end
	a.srv.mu.Unlock()
	if _, err := a.srv.Pause(context.Background(), 50*time.Millisecond); !errors.HasCode(err, ErrBusy) {
		t.Fatalf("paused over a run: %v", err)
	}
	a.srv.mu.Lock()
	a.srv.active--
	paused := a.srv.paused
	a.srv.mu.Unlock()
	if paused != 0 {
		t.Fatalf("a failed pause held %d", paused)
	}
}

func TestPauseIdempotentAndWaitsForHeld(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ok"))})}})
	resume, err := a.srv.Pause(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resume()
	resume() // double call must not underflow paused
	resume2, err := a.srv.Pause(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.srv.hold("c", holdRead); !errors.HasCode(err, ErrBusy) {
		t.Fatalf("hold while paused: %v", err)
	}
	resume2()

	rel, err := a.srv.hold("c", holdRead)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	if _, err := a.srv.Pause(context.Background(), 50*time.Millisecond); !errors.HasCode(err, ErrBusy) {
		t.Fatalf("pause over hold: %v", err)
	}
}

// Outside a container the API refuses an address that is not loopback;
// in one it takes it.
func TestContainerListens(t *testing.T) {
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skip("no interface to listen on")
	}
	defer func() { _ = l.Close() }()
	srv, err := New(Config{Token: token, NoStore: true, Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(context.Background(), l); !errors.HasCode(err, ErrConfig) {
		t.Fatalf("outside a container: %v", err)
	}
	srv.Close()
	in, err := New(Config{Token: token, NoStore: true, Container: true, Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- in.Serve(ctx, l) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("in a container: %v", err)
	}
}

// In a container the API answers the loopback and its peers only: the
// Host header says nothing of where a request comes from.
func TestContainerPeers(t *testing.T) {
	srv, err := New(Config{Token: token, NoStore: true, Container: true, Peers: []netip.Prefix{netip.MustParsePrefix("172.17.0.1/32")},
		Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h := srv.Handler()
	for remote, want := range map[string]int{
		"127.0.0.1:5000":           http.StatusOK,
		"172.17.0.1:5000":          http.StatusOK,
		"[::ffff:172.17.0.1]:5000": http.StatusOK,
		"172.17.0.3:5000":          http.StatusForbidden,
		"192.168.1.20:5000":        http.StatusForbidden,
		"[::1]:5000":               http.StatusOK,
	} {
		req := httptest.NewRequest("GET", "/health", nil)
		req.Host, req.RemoteAddr = "127.0.0.1:8765", remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", remote, rec.Code, want)
		}
	}
	g := srv.Guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest("GET", "/a2a", nil)
	req.Host, req.RemoteAddr = "127.0.0.1:8765", "172.17.0.3:5000"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("direct Guard from non-peer: %d, want 403", rec.Code)
	}
}
