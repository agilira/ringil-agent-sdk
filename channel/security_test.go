// security_test.go: fuzz and authorization/rate-limit security tests for channel bridge
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
)

func FuzzNormalPhoneAndSafeName(f *testing.F) {
	seeds := []string{
		"+39 333 123 4567",
		"0039-333.1234567",
		"+44 (0)20 7946 0958",
		"../../etc/passwd",
		`..\..\win.ini`,
		".env",
		"...",
		"a\x00b",
		strings.Repeat("a", 120) + ".pdf",
		"/automate@bot daily",
		"/automate2 nope",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 1024 {
			return
		}
		checkPhoneInvariants(t, s)
		checkSafeNameInvariants(t, s)
		checkAutomationInvariants(t, s)
	})
}

func checkPhoneInvariants(t *testing.T, s string) {
	t.Helper()
	for _, marked := range []bool{false, true} {
		got, ok := normalPhone(s, marked)
		if !ok {
			continue
		}
		if len(got) < 8 || len(got) > 15 || got[0] == '0' || strings.Trim(got, "0123456789") != "" {
			t.Fatalf("normalPhone(%q, %v) returned invalid digits %q", s, marked, got)
		}
		if marked && strings.Contains(s, "(0)") {
			t.Fatalf("normalPhone(%q, true) accepted trunk (0)", s)
		}
	}
}

func checkSafeNameInvariants(t *testing.T, s string) {
	t.Helper()
	sn := safeName(s)
	if sn == "" || sn == "." || sn == ".." {
		t.Fatalf("safeName(%q) returned %q", s, sn)
	}
	if strings.ContainsAny(sn, "/\\\x00") || utf8.RuneCountInString(sn) > 80 {
		t.Fatalf("safeName(%q) returned unsafe %q", s, sn)
	}
	if strings.ContainsFunc(sn, unicode.IsControl) {
		t.Fatalf("safeName(%q) kept control rune in %q", s, sn)
	}
}

func checkAutomationInvariants(t *testing.T, s string) {
	t.Helper()
	if _, ok := automation(s); !ok {
		return
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		t.Fatalf("automation(%q) matched empty text", s)
	}
	word, _, _ := strings.Cut(strings.ToLower(fields[0]), "@")
	if word != "/automate" {
		t.Fatalf("automation(%q) matched non-automate command %q", s, word)
	}
}

func TestSecurityApprovalPrivilegeAndReplay(t *testing.T) {
	r := rigWith(t, Config{Members: []string{"200"}, MemberProfile: "default"}, true,
		fake.Call("delete_file", "{}"), fake.Reply("done"))

	r.app.from(Message{Chat: "-50", From: "200", Text: "go", Group: true, Addressed: true})
	prompt := r.app.nextIn(t, "-50")
	if len(prompt.Buttons) == 0 {
		t.Fatalf("expected approval buttons, got %+v", prompt)
	}
	tok, _, _ := strings.Cut(prompt.Buttons[0].ID, ":")

	r.app.from(Message{Chat: "-50", From: "200", Pressed: tok + ":y", Group: true})
	if got := r.app.nextIn(t, "-50").Text; !strings.Contains(got, "owner") {
		t.Fatalf("member press got %q", got)
	}
	if r.ran.Load() {
		t.Fatal("member approved a tool call")
	}

	r.app.from(Message{Chat: "999", From: me, Pressed: tok + ":y"})
	if got := r.app.nextIn(t, "999").Text; !strings.Contains(got, "closed") {
		t.Fatalf("cross-chat press got %q", got)
	}
	if r.ran.Load() {
		t.Fatal("cross-chat token press approved a tool call")
	}

	r.app.from(Message{Chat: "-50", From: me, Pressed: tok + ":y", Group: true})
	_ = r.app.nextIn(t, "-50")
	_ = r.app.nextIn(t, "-50")
	if !r.ran.Load() {
		t.Fatal("owner approval did not execute tool")
	}
}

func TestSecurityPrivateMemberAndAskEveryTime(t *testing.T) {
	r := rigWith(t, Config{Members: []string{"200"}, MemberProfile: "default"}, true,
		fake.Call("delete_file", "{}"), fake.Reply("blocked"))
	r.app.from(Message{Chat: "200", From: "200", Text: "delete"})
	got := r.app.nextIn(t, "200")
	_ = r.app.nextIn(t, "200")
	if r.ran.Load() || !strings.Contains(got.Text, "owner") {
		t.Fatalf("private member ran=%v msg=%q", r.ran.Load(), got.Text)
	}

	b := r.bridge
	b.mu.Lock()
	b.chats["skill-chat"] = &chat{conv: "c1", run: "r1", role: owner}
	b.mu.Unlock()
	b.ask(context.Background(), "skill-chat", "r1", serve.Approval{ID: "a1", Tool: "create_skill", Input: json.RawMessage(`{"name":"s"}`)})
	out := r.app.nextIn(t, "skill-chat")
	tok, _, _ := strings.Cut(out.Buttons[0].ID, ":")
	b.press(context.Background(), Message{Chat: "skill-chat", From: me, Pressed: tok + ":a"}, owner)
	_ = r.app.nextIn(t, "skill-chat")

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.always["c1\x00create_skill"] {
		t.Fatal("askEveryTime tool was recorded in always map")
	}
}

func TestSecurityAutomateRestrictedToOwnerPrivateChat(t *testing.T) {
	r := rigWith(t, Config{Members: []string{"200"}, MemberProfile: "default"}, true, fake.Reply("should not run"))
	r.app.from(Message{Chat: "200", From: "200", Text: "/automate every day check"})
	if got := r.app.nextIn(t, "200").Text; !strings.Contains(got, "owner") {
		t.Fatalf("member /automate got %q", got)
	}
	r.app.from(Message{Chat: "-50", From: me, Text: "/automate every day check", Group: true, Addressed: true})
	if got := r.app.nextIn(t, "-50").Text; !strings.Contains(got, "owner") {
		t.Fatalf("group /automate got %q", got)
	}
	if r.model.Remaining() != 1 {
		t.Fatal("unauthorized /automate reached the model")
	}
}

func TestSecurityRateLimitBoundedMemory(t *testing.T) {
	r := newRig(t, true)
	b := r.bridge
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	for i := range maxTracked {
		if !b.within("stranger-"+strconv.Itoa(i), stranger) {
			t.Fatalf("stranger %d refused before cap", i)
		}
	}
	for i := range 50 {
		if b.within("flood-"+strconv.Itoa(i), stranger) {
			t.Fatalf("flood stranger %d allowed past maxTracked", i)
		}
	}
	b.mu.Lock()
	tracked := len(b.recent)
	b.mu.Unlock()
	if tracked > maxTracked {
		t.Fatalf("tracked=%d exceeds maxTracked=%d", tracked, maxTracked)
	}
	if !b.within(me, owner) {
		t.Fatal("owner was rate-limited")
	}
}

func TestSecuritySharedPhonesPrunedOnRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.json")
	initial := `{"u1":"393331112222","u2":"393339998888"}`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _, _ := phoneBridge(t, path, "+39 333 111 2222")
	if !b.phoneMember("u1") || b.phoneMember("u2") {
		t.Fatalf("u1=%v u2=%v", b.phoneMember("u1"), b.phoneMember("u2"))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "u2") {
		t.Fatalf("revoked phone u2 remained on disk: %s", data)
	}
}
