// webhook.go: the endpoint that sets off Webhook triggers
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"
)

// Webhook requests: POST {WebhookPath}{name}, the body signed with the
// trigger's Secret as GitHub signs its own (HMAC-SHA256 of the body, in
// hex, in the SignatureHeader as "sha256=..."), so that most services
// that send webhooks can call it as they are.
const (
	WebhookPath     = "/hooks/"
	SignatureHeader = "X-Hub-Signature-256"
	// MaxWebhookBody bounds a request's body; what the run is told of it
	// is clipped further, to MaxDetail.
	MaxWebhookBody = 256 << 10
)

// replayWindow is how long a delivery is remembered, so that the same
// request sent again (by an attacker who saw it, or a retry of one that
// arrived) does not set the trigger off twice; maxSeen bounds them.
const (
	replayWindow = 24 * time.Hour
	maxSeen      = 4096
)

// WebhookHandler answers POST {WebhookPath}{name} for the runner's
// Webhook triggers. A request whose signature does not check, or for a
// name that is not a webhook, is refused the same way (401, in the same
// time): nothing is told of which triggers exist. One accepted is
// answered 202 when its run starts, 409 when the trigger is paused or
// outside its period, 503 when the triggers are not running (a retry may
// run it). The same body, signed, sets a trigger off once a day (409
// after): a request seen cannot be sent again by whoever saw it, so a
// sender should put something of its own in each (an ID, a time). What
// was delivered is kept in memory: a restart forgets it. Serve it on a listener of its own,
// behind TLS when it faces the internet.
func (s *Runner) WebhookHandler() http.Handler {
	h := &hooks{runner: s, seen: map[string]time.Time{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+WebhookPath+"{name}", h.serve)
	return mux
}

type hooks struct {
	runner *Runner
	mu     sync.Mutex
	seen   map[string]time.Time // deliveries (name and MAC), by when
}

func (h *hooks) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBody))
	var tooLarge *http.MaxBytesError
	switch {
	case stderrors.As(err, &tooLarge):
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		http.Error(w, "cannot read the body", http.StatusBadRequest)
		return
	}
	if !h.runner.live() { // starting or stopping: its triggers are not known yet
		http.Error(w, "not running", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	mac, ok := h.signed(name, r.Header.Get(SignatureHeader), body)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	key := name + "\x00" + hex.EncodeToString(mac) // the MAC, not its spelling: "AB" and "ab" are one delivery
	if !h.first(key) {
		http.Error(w, "delivered already: the same body is taken once a day", http.StatusConflict)
		return
	}
	err = h.runner.Fire(name, string(body))
	if err != nil {
		h.forgetKey(key) // not run: a retry may run it
	}
	answerFire(w, err)
}

// answerFire says how Fire went: 202 started, 503 not running, 409 not now.
func answerFire(w http.ResponseWriter, err error) {
	var e *errors.Error
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case stderrors.As(err, &e) && e.Code == ErrStopped:
		http.Error(w, "not running", http.StatusServiceUnavailable)
	default:
		http.Error(w, "not now", http.StatusConflict)
	}
}

// dummyKey is what a request for no webhook is checked against, so that
// it costs what a real one does: its time tells no names.
var dummyKey = []byte("ringil-no-such-webhook-0123456789abcdef")

// signed checks sig, the HMAC of body with the secret of Webhook trigger
// name, and returns the MAC.
func (h *hooks) signed(name, sig string, body []byte) ([]byte, bool) {
	secret, known := h.runner.secretOf(name)
	key := []byte(secret)
	if !known {
		key = dummyKey
	}
	want := hmac.New(sha256.New, key)
	want.Write(body)
	sum := want.Sum(nil)
	got, ok := strings.CutPrefix(sig, "sha256=")
	if !ok {
		return nil, false
	}
	mac, err := hex.DecodeString(got)
	if err != nil {
		return nil, false
	}
	return mac, hmac.Equal(mac, sum) && known
}

// first records a delivery; false when it was seen within the window.
func (h *hooks) first(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if at, ok := h.seen[key]; ok && now.Sub(at) < replayWindow {
		return false
	}
	if len(h.seen) >= maxSeen {
		h.forget(now)
	}
	h.seen[key] = now
	return true
}

// forgetKey drops a delivery that did not run.
func (h *hooks) forgetKey(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.seen, key)
}

// forget drops the deliveries out of the window, then, if still full,
// the oldest. h.mu held.
func (h *hooks) forget(now time.Time) {
	oldest, at := "", now
	for sig, t := range h.seen {
		if now.Sub(t) >= replayWindow {
			delete(h.seen, sig)
			continue
		}
		if t.Before(at) {
			oldest, at = sig, t
		}
	}
	if len(h.seen) >= maxSeen {
		delete(h.seen, oldest)
	}
}

// secretOf is the Secret of Webhook trigger name, as the runner holds it.
func (s *Runner) secretOf(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.triggers[name]
	if !ok || p.trigger.kind() != Webhook || p.trigger.Secret == "" {
		return "", false
	}
	return p.trigger.Secret, true
}

// live: the runner runs (Run took its triggers and has not ended).
func (s *Runner) live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx != nil
}
