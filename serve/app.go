// app.go: an app in the browser, served by the server itself
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// Sign-in and sessions of the app.
const (
	sessionCookie = "ringil_session"
	codeLife      = 10 * time.Minute
	sessionLife   = 7 * 24 * time.Hour
	maxSessions   = 64
	maxCodes      = 16
)

// appAuth holds the sign-in codes and the sessions they opened, by their
// hashes: a copy of the server's memory gives neither away.
type appAuth struct {
	mu       sync.Mutex
	codes    map[[32]byte]time.Time // until
	sessions map[[32]byte]time.Time // until
}

func newAppAuth() *appAuth {
	return &appAuth{codes: map[[32]byte]time.Time{}, sessions: map[[32]byte]time.Time{}}
}

func secret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand does not fail
	return hex.EncodeToString(b)
}

// SignInPath is a path that signs a browser in to the app, once, within
// ten minutes: the host opens or prints it with its address
// ("http://127.0.0.1:8080" + path). "" when the server has no app.
func (s *Server) SignInPath() string {
	if s.cfg.App == nil {
		return ""
	}
	code := secret()
	a := s.app
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prune(s.now())
	if len(a.codes) >= maxCodes {
		clear(a.codes) // an earlier link no longer works: ask a new one
	}
	a.codes[sha256.Sum256([]byte(code))] = s.now().Add(codeLife)
	return "/signin?code=" + code
}

// prune drops what has expired.
func (a *appAuth) prune(now time.Time) {
	for k, until := range a.codes {
		if now.After(until) {
			delete(a.codes, k)
		}
	}
	for k, until := range a.sessions {
		if now.After(until) {
			delete(a.sessions, k)
		}
	}
}

// signIn turns a code into a session, once.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		failure(w, http.StatusMethodNotAllowed, fail("SERVE_METHOD", "sign-in uses GET"))
		return
	}
	a := s.app
	key := sha256.Sum256([]byte(r.URL.Query().Get("code")))
	now := s.now()
	a.mu.Lock()
	until, ok := a.codes[key]
	delete(a.codes, key)
	var token string
	if ok && now.Before(until) {
		a.prune(now)
		if len(a.sessions) >= maxSessions {
			a.dropOldest()
		}
		token = secret()
		a.sessions[sha256.Sum256([]byte(token))] = now.Add(sessionLife)
	}
	a.mu.Unlock()
	if token == "" {
		failure(w, http.StatusForbidden, fail("SERVE_AUTH", "the sign-in link is used or expired: ask the host for a new one"))
		return
	}
	// #nosec G124 -- loopback over plain HTTP, where Safari refuses a Secure cookie; HttpOnly, SameSite=Strict and the origin check (signedIn) guard it
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLife / time.Second)})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// dropOldest ends the session that would expire first.
func (a *appAuth) dropOldest() {
	var oldest [32]byte
	var at time.Time
	for k, until := range a.sessions {
		if at.IsZero() || until.Before(at) {
			oldest, at = k, until
		}
	}
	delete(a.sessions, oldest)
}

// signOut ends the browser's session.
func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	s.app.mu.Lock()
	for _, c := range r.CookiesNamed(sessionCookie) {
		delete(s.app.sessions, sha256.Sum256([]byte(c.Value)))
	}
	s.app.mu.Unlock()
	// #nosec G124 -- clears the cookie above, with its attributes
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// signedIn says whether r comes from the app's own page with a live
// session. The cookie alone is not enough: for a browser another port of
// the same address is the same site, so it gets the cookie too; the
// request must come from this very origin. Every cookie of the name is
// tried: a page on another port can set one of its own, which the
// browser sends first.
func (s *Server) signedIn(r *http.Request) bool {
	if s.cfg.App == nil || !sameOrigin(r) {
		return false
	}
	cookies := r.CookiesNamed(sessionCookie)
	if len(cookies) > 32 { // the browser's own bound is near: a page cannot push ours past it
		cookies = cookies[:32]
	}
	now := s.now()
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	for _, c := range cookies {
		if c.Value == "" {
			continue
		}
		if until, ok := s.app.sessions[sha256.Sum256([]byte(c.Value))]; ok && now.Before(until) {
			return true
		}
	}
	return false
}

// sameOrigin: the browser says the request comes from a page of this
// origin (scheme, host and port), by Sec-Fetch-Site or by Origin.
func sameOrigin(r *http.Request) bool {
	own := "http://" + r.Host
	origin := r.Header.Get("Origin")
	if origin != "" && subtle.ConstantTimeCompare([]byte(origin), []byte(own)) != 1 {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "":
		return origin != "" // an older browser: its Origin is ours
	}
	return false
}

// appFiles serves the app's files; a path that is none of them is the
// app's own route, answered with its index.html.
func (s *Server) appFiles() http.Handler {
	files := http.FileServerFS(s.cfg.App)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			failure(w, http.StatusMethodNotAllowed, fail("SERVE_METHOD", "the app's files are read with GET"))
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "."
		}
		if info, err := fs.Stat(s.cfg.App, name); err != nil || (name != "." && info.IsDir()) {
			r = r.Clone(r.Context())
			r.URL.Path = "/" // the app's router takes it from there
		}
		files.ServeHTTP(w, r)
	})
}
