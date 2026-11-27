// apicheck_test.go: what the app's reference found uneven, 27/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/session/file"
)

// A session kept in its file outlives the server: a restart does not
// sign the browser out.
func TestSessionsOutliveARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "app-sessions.json")
	cfg := Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ok"))})}, App: appFS, Sessions: file}
	a := newAPI(t, cfg)
	cookie := a.signedIn()
	b := newAPI(t, cfg) // the server started again
	if resp := b.raw("GET", "/v1/profiles", "Cookie", cookie, "Sec-Fetch-Site", "same-origin"); resp.StatusCode != http.StatusOK {
		t.Fatalf("after a restart: %d %s", resp.StatusCode, body(resp))
	}
	if resp := b.raw("POST", "/v1/signout", "Cookie", cookie, "Sec-Fetch-Site", "same-origin"); resp.StatusCode != http.StatusNoContent {
		t.Fatal("sign out")
	}
	c := newAPI(t, cfg)
	if resp := c.raw("GET", "/v1/profiles", "Cookie", cookie, "Sec-Fetch-Site", "same-origin"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed out, then a restart: %d", resp.StatusCode)
	}
}

// The app's page without a session gets one answer, 401 signed out,
// whatever the method.
func TestSignedOutIsOneAnswer(t *testing.T) {
	a := appAPI(t)
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		resp := a.raw(method, "/v1/conversations", "Origin", a.url, "Sec-Fetch-Site", "same-origin")
		if b := body(resp); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(b, string(ErrSignedOut)) {
			t.Errorf("%s: %d %s", method, resp.StatusCode, b)
		}
	}
	if resp := a.raw("POST", "/v1/runs", "Origin", "https://evil.example.com", "Sec-Fetch-Site", "cross-site"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("another page: %d", resp.StatusCode)
	}
}

// What no route answers under /v1 is JSON; a bad conversation ID says why.
func TestErrorsSayWhy(t *testing.T) {
	a := storedAPI(t)
	code, out := a.do("GET", "/v1/no-such-thing", "")
	if code != http.StatusNotFound || !strings.Contains(out, `"error"`) {
		t.Fatalf("%d %s", code, out)
	}
	code, out = a.do("GET", "/v1/conversations/bad%20id/messages", "")
	if code != http.StatusBadRequest || !strings.Contains(out, "session ID must be") {
		t.Fatalf("%d %s", code, out)
	}
}

// The host refuses a run on a project it has not, at once.
func TestCheckProject(t *testing.T) {
	cfg := Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ok"))})},
		CheckProject: func(_ context.Context, p string) error {
			if p != "q1" {
				return errors.New("PROJECT", "no project "+p).WithUserMessage("no project " + p)
			}
			return nil
		}}
	a := newAPI(t, cfg)
	if _, err := a.srv.StartFrom(Request{Profile: "default", Project: "nope", Input: agent.UserText("x")}); status(err) != http.StatusBadRequest || !strings.Contains(userMessage(err), "no project nope") {
		t.Fatalf("%v", err)
	}
	if _, err := a.srv.StartFrom(Request{Profile: "default", Project: "q1", Input: agent.UserText("x")}); err != nil {
		t.Fatal(err)
	}
}

// A model event names the model that answered.
func TestWireNamesTheModel(t *testing.T) {
	if w := wire(agent.Event{Kind: agent.EventModel, Model: "nemotron"}); w.Model != "nemotron" {
		t.Fatalf("%+v", w)
	}
}

// A follow-up run on a conversation whose project is later rejected
// is refused at once, even when the request omits the project.
func TestCheckProjectFollowUp(t *testing.T) {
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	allow := true
	cfg := Config{
		Store:    store,
		Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ok"))})},
		CheckProject: func(_ context.Context, p string) error {
			if !allow {
				return errors.New("PROJECT", "archived "+p).WithUserMessage("archived " + p)
			}
			return nil
		},
	}
	a := newAPI(t, cfg)
	snap, err := a.srv.StartFrom(Request{Profile: "default", Conversation: "c1", Project: "q1", Input: agent.UserText("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.srv.Wait(context.Background(), snap.ID); err != nil {
		t.Fatal(err)
	}
	allow = false
	if _, err := a.srv.StartFrom(Request{Profile: "default", Conversation: "c1", Input: agent.UserText("again")}); status(err) != http.StatusBadRequest || !strings.Contains(userMessage(err), "archived q1") {
		t.Fatalf("follow-up not refused: %v", err)
	}
}

// Sessions kept in a folder open to other users are refused at restart.
func TestSessionsRefuseOpenDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	dir := filepath.Join(t.TempDir(), "state")
	file := filepath.Join(dir, "app-sessions.json")
	cfg := Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ok"))})}, App: appFS, Sessions: file}
	a := newAPI(t, cfg)
	cookie := a.signedIn()
	if err := os.Chmod(dir, 0o777); err != nil { // #nosec G302 -- intentional test for open-directory refusal
		t.Fatal(err)
	}
	b := newAPI(t, cfg)
	if resp := b.raw("GET", "/v1/profiles", "Cookie", cookie, "Sec-Fetch-Site", "same-origin"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("loaded session from open dir: %d", resp.StatusCode)
	}
}

func sessionsCfg(t *testing.T, file string) Config {
	return Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ok"))})}, App: appFS, Sessions: file}
}

func (a *api) withCookie(cookie string) int {
	a.t.Helper()
	return a.raw("GET", "/v1/profiles", "Cookie", cookie, "Sec-Fetch-Site", "same-origin").StatusCode
}

// Two servers on one sessions file: a sign-out on one is not undone by a
// sign-in on the other.
func TestSessionsOfTwoServers(t *testing.T) {
	file := filepath.Join(t.TempDir(), "s", "app-sessions.json")
	a, b := newAPI(t, sessionsCfg(t, file)), newAPI(t, sessionsCfg(t, file))
	c1 := a.signedIn()
	b2 := newAPI(t, sessionsCfg(t, file)) // sees c1 at its start
	if resp := b2.raw("POST", "/v1/signout", "Cookie", c1, "Sec-Fetch-Site", "same-origin"); resp.StatusCode != http.StatusNoContent {
		t.Fatal("sign out")
	}
	_ = a.signedIn() // a writes the file again
	_ = b
	if code := newAPI(t, sessionsCfg(t, file)).withCookie(c1); code != http.StatusUnauthorized {
		t.Fatalf("a session signed out came back: %d", code)
	}
}

// A sign-out that cannot be kept is said.
func TestSignOutNotKeptIsSaid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("folder modes")
	}
	dir := filepath.Join(t.TempDir(), "s")
	a := newAPI(t, sessionsCfg(t, filepath.Join(dir, "app-sessions.json")))
	cookie := a.signedIn()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("no modes")
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	if resp := a.raw("POST", "/v1/signout", "Cookie", cookie, "Sec-Fetch-Site", "same-origin"); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a sign-out not kept answered %d", resp.StatusCode)
	}
}

// A session no sign-in made (too far ahead), or a file others may write,
// gives no access.
func TestSessionsFileTrusted(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "app-sessions.json")
	token := strings.Repeat("ab", 32)
	h := sha256.Sum256([]byte(token))
	far := `[{"hash":"` + hex.EncodeToString(h[:]) + `","until":"2126-01-01T00:00:00Z"}]`
	if err := os.WriteFile(file, []byte(far), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := newAPI(t, sessionsCfg(t, file)).withCookie(sessionCookie + "=" + token); code != http.StatusUnauthorized {
		t.Fatalf("a session 100 years ahead: %d", code)
	}
	if runtime.GOOS == "windows" {
		return
	}
	near := `[{"hash":"` + hex.EncodeToString(h[:]) + `","until":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}]`
	if err := os.WriteFile(file, []byte(near), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(file, 0o646)
	if code := newAPI(t, sessionsCfg(t, file)).withCookie(sessionCookie + "=" + token); code != http.StatusUnauthorized {
		t.Fatalf("a file others may write: %d", code)
	}
}

// The token's holder gets in whatever Sec-Fetch-Site says; a wrong method
// is a JSON 405 with Allow; a host may mount /v1/ itself.
func TestTokenMethodsAndMounts(t *testing.T) {
	a := appAPI(t)
	if code, out := a.do("GET", "/v1/profiles", "", "Sec-Fetch-Site", "same-origin"); code != http.StatusOK {
		t.Fatalf("the token with Sec-Fetch-Site: %d %s", code, out)
	}
	resp := a.raw("POST", "/v1/profiles", "Authorization", "Bearer "+token)
	if b := body(resp); resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") == "" || !strings.Contains(b, `"error"`) {
		t.Fatalf("%d %q %s", resp.StatusCode, resp.Header.Get("Allow"), b)
	}
	b := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	b.srv.Mount("/v1/", http.NotFoundHandler())
	_ = b.srv.Handler() // no panic
}
