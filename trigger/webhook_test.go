// webhook_test.go: the endpoint that sets off Webhook triggers
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var hookSecret = strings.Repeat("k", 40)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func post(h http.Handler, path, sig, body string) int {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if sig != "" {
		r.Header.Set(SignatureHeader, sig)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func TestAWebhookSignedSetsItOffOnce(t *testing.T) {
	store, clock, rec := harness(t, Trigger{Name: "deploy", Kind: Webhook, Secret: hookSecret, Skill: "deploy"})
	h := launch(t, store, clock, rec).WebhookHandler()
	body := `{"ref":"refs/heads/main"}`
	if code := post(h, "/hooks/deploy", sign(hookSecret, body), body); code != http.StatusAccepted {
		t.Fatalf("code %d", code)
	}
	eventually(t, "no run", func() bool { return rec.count() == 1 })
	if f := rec.lastFired(); f.Kind != Webhook || f.Detail != body {
		t.Fatalf("%+v", f)
	}
	if code := post(h, "/hooks/deploy", sign(hookSecret, body), body); code != http.StatusConflict {
		t.Fatalf("replay: code %d", code)
	}
	if code := post(h, "/hooks/deploy", strings.ToUpper(sign(hookSecret, body)[7:]), body); code != http.StatusUnauthorized {
		t.Fatalf("no prefix: code %d", code)
	}
	if code := post(h, "/hooks/deploy", "sha256="+strings.ToUpper(sign(hookSecret, body)[7:]), body); code != http.StatusConflict {
		t.Fatalf("the replay in capitals: code %d", code) // audit 20/11: the spelling of the hex made it another delivery
	}
	other := `{"ref":"refs/heads/dev"}`
	if code := post(h, "/hooks/deploy", sign(hookSecret, other), other); code != http.StatusAccepted {
		t.Fatalf("code %d", code)
	}
	eventually(t, "the second delivery did not run", func() bool { return rec.count() == 2 })
	if rec.count() != 2 {
		t.Fatalf("the replay ran: %d runs", rec.count())
	}
}

func TestAWebhookRefusesWhatIsNotSigned(t *testing.T) {
	store, clock, rec := harness(t,
		Trigger{Name: "deploy", Kind: Webhook, Secret: hookSecret, Skill: "deploy"},
		Trigger{Name: "urgent", Kind: Message, Chat: "telegram-5", Prompt: "tell me"},
		trigger("daily", "@daily"))
	h := launch(t, store, clock, rec).WebhookHandler()
	body := `{"x":1}`
	for _, tc := range []struct{ name, path, sig string }{
		{"no signature", "/hooks/deploy", ""},
		{"wrong secret", "/hooks/deploy", sign(strings.Repeat("x", 40), body)},
		{"another body", "/hooks/deploy", sign(hookSecret, `{"x":2}`)},
		{"no prefix", "/hooks/deploy", strings.TrimPrefix(sign(hookSecret, body), "sha256=")},
		{"not hex", "/hooks/deploy", "sha256=zz"},
		{"no such trigger", "/hooks/nope", sign(hookSecret, body)},
		{"not a webhook", "/hooks/urgent", sign(hookSecret, body)},
		{"a time trigger", "/hooks/daily", sign(hookSecret, body)},
	} {
		if code := post(h, tc.path, tc.sig, body); code != http.StatusUnauthorized {
			t.Errorf("%s: code %d", tc.name, code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/hooks/deploy", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
	big := strings.Repeat("a", MaxWebhookBody+1)
	if code := post(h, "/hooks/deploy", sign(hookSecret, big), big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d", code)
	}
	if rec.count() != 0 {
		t.Fatalf("%d runs", rec.count())
	}
}

func TestAPausedWebhookIsNotNow(t *testing.T) {
	store, clock, rec := harness(t, Trigger{Name: "deploy", Kind: Webhook, Secret: hookSecret, Skill: "deploy", Paused: true})
	h := launch(t, store, clock, rec).WebhookHandler()
	if code := post(h, "/hooks/deploy", sign(hookSecret, "{}"), "{}"); code != http.StatusConflict {
		t.Fatalf("code %d", code)
	}
}

func TestDeliveriesSeenAreBounded(t *testing.T) {
	h := &hooks{seen: map[string]map[string]time.Time{}}
	for i := range maxSeen + 10 {
		if !h.first("deploy", "sig-"+strconv.Itoa(i)) {
			t.Fatalf("%d seen", i)
		}
	}
	if n := len(h.seen["deploy"]); n > maxSeen {
		t.Fatalf("%d remembered", n)
	}
	if h.first("deploy", "sig-"+strconv.Itoa(maxSeen+9)) {
		t.Fatal("the newest forgotten")
	}
}

// Audit 20/11: a delivery refused while the triggers were not running
// was kept as delivered, and the sender's retry was answered 202 with no
// run.
func TestARefusedDeliveryMayBeSentAgain(t *testing.T) {
	store, _, rec := harness(t, Trigger{Name: "deploy", Kind: Webhook, Secret: hookSecret, Skill: "deploy"})
	s, err := New(Config{Store: store, Run: rec.run})
	if err != nil {
		t.Fatal(err)
	}
	h := s.WebhookHandler() // not running yet
	for range 2 {
		if code := post(h, "/hooks/deploy", sign(hookSecret, "{}"), "{}"); code != http.StatusServiceUnavailable {
			t.Fatalf("code %d", code)
		}
	}
}

// A trigger's own flood of deliveries must not evict another trigger's
// replay records (regression: the seen set was global, so one trigger with a
// valid Secret could re-enable replay of a captured delivery of another).
func TestReplayIsolatedPerTrigger(t *testing.T) {
	h := &hooks{seen: map[string]map[string]time.Time{}}
	const bMAC = "b-captured-delivery"
	if !h.first("b", bMAC) {
		t.Fatal("b's first delivery should be new")
	}
	for i := range maxSeen + 100 { // trigger "a" floods its own set
		h.first("a", "a-"+strconv.Itoa(i))
	}
	if h.first("b", bMAC) {
		t.Fatal("a's traffic evicted b's replay protection: the captured delivery would replay")
	}
	if n := len(h.seen["a"]); n > maxSeen {
		t.Fatalf("a's own set is unbounded: %d", n)
	}
}
