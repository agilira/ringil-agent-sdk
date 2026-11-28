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
	"encoding/json"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
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
	file     string                 // where the sessions are kept, "" for none
	gone     map[[32]byte]bool      // signed out here: kept out of the file
	saved    map[[32]byte]bool      // what this server last read or wrote of the file
}

func newAppAuth(file string) *appAuth {
	a := &appAuth{codes: map[[32]byte]time.Time{}, sessions: map[[32]byte]time.Time{}, file: file, gone: map[[32]byte]bool{}, saved: map[[32]byte]bool{}}
	a.load()
	return a
}

// savedSession is a session as its file keeps it.
type savedSession struct {
	Hash  string    `json:"hash"`
	Until time.Time `json:"until"`
}

// load reads the sessions kept, if any: a file private to the user,
// each session at most sessionLife ahead. A file that cannot be read
// signs everyone out, nothing more.
func (a *appAuth) load() {
	for h, until := range a.kept() {
		if len(a.sessions) < maxSessions {
			a.sessions[h] = until
			a.saved[h] = true
		}
	}
}

// kept are the sessions the file holds that are still good.
func (a *appAuth) kept() map[[32]byte]time.Time {
	out := map[[32]byte]time.Time{}
	if a.file == "" {
		return out
	}
	data, err := readPrivateFile(a.file, 64<<10)
	if err != nil {
		return out
	}
	var list []savedSession
	if json.Unmarshal(data, &list) != nil {
		return out
	}
	now := time.Now()
	for _, s := range list {
		b, err := hex.DecodeString(s.Hash)
		if err != nil || len(b) != 32 || !now.Before(s.Until) || s.Until.After(now.Add(sessionLife)) {
			continue // a session no sign-in made (planted, or from a clock gone wrong)
		}
		var h [32]byte
		copy(h[:], b)
		out[h] = s.Until
	}
	return out
}

// save writes the sessions, when they are kept: those of the file (another
// serve's on the same folder) and this one's, less those signed out here.
// The caller holds mu.
func (a *appAuth) save() error {
	if a.file == "" {
		return nil
	}
	all := a.kept()
	for h := range a.gone {
		delete(all, h)
	}
	for h := range a.sessions { // gone from the file since this server wrote it: another serve signed it out
		if _, still := all[h]; a.saved[h] && !still {
			delete(a.sessions, h)
		}
	}
	maps.Copy(all, a.sessions)
	clear(a.saved)
	for h := range all {
		a.saved[h] = true
	}
	list := make([]savedSession, 0, len(all))
	for h, until := range all {
		list = append(list, savedSession{Hash: hex.EncodeToString(h[:]), Until: until})
	}
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return writePrivateFile(a.file, data)
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
		_ = a.save() // a session not kept is a sign-in again after a restart, nothing more
	}
	a.mu.Unlock()
	if token == "" { // a person followed it: a page, not JSON
		usedLink(w)
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
		h := sha256.Sum256([]byte(c.Value))
		delete(s.app.sessions, h)
		s.app.gone[h] = true
	}
	err := s.app.save()
	s.app.mu.Unlock()
	if err != nil { // signed out here, but a restart would sign it in again: said, not hidden (audit 27/11)
		failure(w, http.StatusInternalServerError, fail("SERVE_SESSIONS", "signed out until the server restarts: its sessions file could not be written"))
		return
	}
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

// readPrivateFile reads a file of the server's own, not through a link.
func readPrivateFile(path string, max int64) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	dirInfo, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	if err := lockfile.CheckPrivate(dirInfo); err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := lockfile.CheckPrivate(info); err != nil || !info.Mode().IsRegular() {
		return nil, os.ErrPermission // another's to write, or not a file: not ours to trust
	}
	return atomicfile.Read(root, name, max)
}

// writePrivateFile writes a file of the server's own whole, private to
// the user, in a private folder made if it is not there.
func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return atomicfile.Write(root, filepath.Base(path), data)
}

// usedLinkPage is what a used or expired sign-in link opens: what to do,
// in the app's colours, with nothing to load.
const usedLinkPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Ringil: sign-in link used</title><style>
:root{color-scheme:light dark;--bg:#f8faf9;--fg:#1d2522;--muted:#5b6762;--accent:#1f6b50}
@media (prefers-color-scheme:dark){:root{--bg:#171c1a;--fg:#e6ece9;--muted:#9aa8a2;--accent:#8fd9b6}}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif}
main{max-width:30rem;padding:2rem;text-align:center}h1{font-size:1.5rem;margin:0 0 .75rem}p{color:var(--muted);margin:.5rem 0}
code{font-size:.95em;color:var(--fg)}a{color:var(--accent);font-weight:600}
</style></head><body><main><h1>This sign-in link was used or has expired</h1>
<p>Each link opens Ringil once, within 10 minutes. If you are signed in already, <a href="/">open Ringil</a>.</p>
<p>Otherwise get a new link where Ringil runs: <code>ringil serve link</code></p>
</main></body></html>`

func usedLink(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, usedLinkPage)
}
