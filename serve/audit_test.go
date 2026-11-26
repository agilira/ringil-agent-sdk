// audit_test.go: what the audit of 26/11 found in the API for an app
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"context"
	"net/http"
	"strings"
	"testing"

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
	r1, r2, r3 := held("Chat1", holdRead), held("chat1", holdRead), held("chat1", holdTitle) // two tabs and a rename
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

// Without a store that keeps titles, renaming is "not found", not a
// server error.
func TestRenameWithoutTitles(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	if err := a.srv.Rename(context.Background(), "abc", "x"); status(err) != http.StatusNotFound {
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
