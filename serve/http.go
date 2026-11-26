// http.go: the HTTP API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	stderrors "errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// maxBody bounds a request body.
const maxBody = 1 << 20

// heartbeat keeps an idle event stream open through proxies and tells a
// reader the server is alive.
var heartbeat = 15 * time.Second

// writeTimeout bounds one write to a stream's reader.
var writeTimeout = 30 * time.Second

// Handler is the API:
//
//	GET    /v1/profiles                          the profiles
//	POST   /v1/runs                              {"profile","input","conversation"?,"attachments"?} starts a run;
//	                                             attachments: [{"mime","data"}], data in base64, images and PDFs
//	GET    /v1/runs                              the runs kept
//	GET    /v1/runs/{id}                         a run
//	GET    /v1/runs/{id}/events                  its events, as Server-Sent Events (Last-Event-ID or ?after= to resume)
//	POST   /v1/runs/{id}/cancel                  stop it
//	POST   /v1/runs/{id}/approvals/{approval}    {"allow": true|false} decide a waiting tool call
//	DELETE /v1/runs/{id}                         forget a finished run
//	GET    /v1/conversations                     the conversations kept, the latest first
//	GET    /v1/conversations/{id}/messages       a page of its messages (?from=&limit=; the last 200 by default)
//	PATCH  /v1/conversations/{id}                {"title"} rename it
//	DELETE /v1/conversations/{id}                delete it
//	GET    /v1/audio                             {"transcribe","speech"} which halves of the voice are on
//	POST   /v1/audio/transcriptions              the recording as the body (audio/webm, ogg, mpeg, wav, mp4) → {"text"}
//	POST   /v1/audio/speech                      {"text"} → the audio, its type in Content-Type
//	POST   /v1/signin-links                      {"path"} a new sign-in link of the app (with Config.App)
//	POST   /v1/signout                           end the browser's session
//	GET    /signin?code=                         the app's sign-in: a session cookie, then the app; no token needed
//	GET    /...                                  the app's files (with Config.App); no token needed
//	GET    /health                               {"status":"ok"}, or 503 {"status":"closing"}; no token needed
//
// /health tells a supervisor (systemd, Docker, a load balancer on this
// machine) whether the server answers, and nothing else: it is the one
// path served without the token, still to the loopback name only and
// not to web pages.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/profiles", func(w http.ResponseWriter, _ *http.Request) { respond(w, http.StatusOK, s.Profiles()) })
	mux.HandleFunc("POST /v1/runs", s.start)
	mux.HandleFunc("GET /v1/runs", func(w http.ResponseWriter, _ *http.Request) { respond(w, http.StatusOK, s.List()) })
	mux.HandleFunc("GET /v1/runs/{id}", s.get)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.events)
	mux.HandleFunc("POST /v1/runs/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /v1/runs/{id}/approvals/{approval}", s.decide)
	mux.HandleFunc("DELETE /v1/runs/{id}", s.forget)
	mux.HandleFunc("GET /v1/conversations", s.listConversations)
	mux.HandleFunc("GET /v1/conversations/{id}/messages", s.history)
	mux.HandleFunc("PATCH /v1/conversations/{id}", s.rename)
	mux.HandleFunc("DELETE /v1/conversations/{id}", s.deleteConversation)
	mux.HandleFunc("GET /v1/audio", s.audioInfo)
	mux.HandleFunc("POST /v1/audio/transcriptions", s.transcribe)
	mux.HandleFunc("POST /v1/audio/speech", s.speak)
	mux.HandleFunc("POST /v1/signin-links", s.signInLink)
	mux.HandleFunc("POST /v1/signout", s.signOut)
	s.mu.Lock()
	for pattern, h := range s.mounts {
		mux.Handle(pattern, h)
	}
	s.mu.Unlock()
	guarded := s.Guard(mux)
	if s.cfg.App == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				s.health(w, r)
				return
			}
			guarded.ServeHTTP(w, r)
		})
	}
	return s.withApp(s.mountMatcher(), guarded)
}

// withApp is the API beside the app: its files and its sign-in, to the
// loopback name only.
func (s *Server) withApp(mounts *http.ServeMux, guarded http.Handler) http.Handler {
	app := s.appFiles()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			s.health(w, r)
		case !loopbackHost(r.Host):
			failure(w, http.StatusForbidden, fail("SERVE_HOST", "requests must name the loopback host"))
		case r.URL.Path == "/signin":
			s.signIn(w, r)
		case !strings.HasPrefix(r.URL.Path, "/v1/") && !s.mounted(mounts, r):
			app.ServeHTTP(w, r)
		default:
			guarded.ServeHTTP(w, r)
		}
	})
}

// mountMatcher builds a path-only mux for mounted handlers so a request
// with a different HTTP method still routes to Guard rather than appFiles.
func (s *Server) mountMatcher() *http.ServeMux {
	matcher := http.NewServeMux()
	seen := map[string]bool{}
	noop := func(http.ResponseWriter, *http.Request) {}
	s.mu.Lock()
	defer s.mu.Unlock()
	for pattern := range s.mounts {
		if _, p, ok := strings.Cut(pattern, " "); ok && p != "" {
			pattern = p
		}
		if !seen[pattern] {
			seen[pattern] = true
			matcher.HandleFunc(pattern, noop)
		}
	}
	return matcher
}

// mounted: a handler the host mounted answers r (an A2A endpoint,
// /metrics): it stays behind the guard, not taken for an app's route.
func (s *Server) mounted(mux *http.ServeMux, r *http.Request) bool {
	_, pattern := mux.Handler(r)
	return pattern != ""
}

// signInLink makes a new sign-in link of the app; bearer token only.
func (s *Server) signInLink(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" || !s.bearerOK(r) {
		failure(w, http.StatusForbidden, fail("SERVE_AUTH", "sign-in links require the bearer token"))
		return
	}
	p := s.SignInPath()
	if p == "" {
		failure(w, http.StatusNotFound, fail(ErrNotFound, "the server has no app"))
		return
	}
	respond(w, http.StatusCreated, map[string]string{"path": p})
}

// health answers whether the server takes runs.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	switch {
	case !loopbackHost(r.Host):
		failure(w, http.StatusForbidden, fail("SERVE_HOST", "requests must name the loopback host"))
	case r.Header.Get("Origin") != "":
		failure(w, http.StatusForbidden, fail("SERVE_ORIGIN", "requests from web pages are refused"))
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		w.Header().Set("Allow", "GET, HEAD")
		failure(w, http.StatusMethodNotAllowed, fail("SERVE_METHOD", "health is read with GET"))
	case s.ctx.Err() != nil:
		w.Header().Set("Cache-Control", "no-store")
		respond(w, http.StatusServiceUnavailable, map[string]string{"status": "closing"})
	default:
		w.Header().Set("Cache-Control", "no-store")
		respond(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func (s *Server) bearerOK(r *http.Request) bool {
	want := []byte("Bearer " + s.cfg.Token)
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) == 1
}

// Guard admits to next only the token's holder, by loopback name, not
// from a browser page: what the API itself requires, for a host mounting
// more on the same listener (an A2A endpoint).
func (s *Server) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !loopbackHost(r.Host):
			// DNS rebinding: a page whose name now points at 127.0.0.1.
			failure(w, http.StatusForbidden, fail("SERVE_HOST", "requests must name the loopback host"))
		case s.signedIn(r): // the app's own page, signed in
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		case r.Header.Get("Origin") != "":
			failure(w, http.StatusForbidden, fail("SERVE_ORIGIN", "requests from web pages are refused"))
		case !s.bearerOK(r):
			w.Header().Set("WWW-Authenticate", "Bearer")
			failure(w, http.StatusUnauthorized, fail("SERVE_AUTH", "a valid bearer token is required"))
		default:
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		}
	})
}

func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type startIn struct {
	Profile      string         `json:"profile"`
	Input        string         `json:"input"`
	Conversation string         `json:"conversation,omitempty"`
	Attachments  []attachmentIn `json:"attachments,omitempty"`
}

// attachmentIn is an image or a PDF sent with the input; its type is its
// MIME's, checked against its bytes.
type attachmentIn struct {
	MIME string `json:"mime"`
	Data []byte `json:"data"` // base64 in the JSON
}

// Bounds of a run's input: a PDF of 20 MiB is about 27 MiB in base64.
const (
	maxAttachments = 20
	maxRunBody     = 64 << 20
)

// message is the input as a user message, its attachments as parts.
func (in startIn) message() (agent.Message, error) {
	switch {
	case len(in.Input) > maxBody: // the room is for attachments, not text
		return agent.Message{}, fail(ErrInput, "the input text is at most 1 MiB")
	case len(in.Attachments) > maxAttachments:
		return agent.Message{}, fail(ErrInput, "at most 20 attachments")
	}
	parts := make([]agent.Part, 0, len(in.Attachments))
	for _, a := range in.Attachments {
		t := agent.PartImage
		if a.MIME == "application/pdf" {
			t = agent.PartDocument
		}
		parts = append(parts, agent.Part{Type: t, MIME: a.MIME, Data: a.Data})
	}
	return agent.UserMessage(in.Input, parts...), nil
}

// upload takes one of the slots for a large body (a run with its
// attachments, a recording): MaxRuns at once, so that bodies read in
// parallel cost at most that many times their bound. false, w answered.
func (s *Server) upload(w http.ResponseWriter) (release func(), ok bool) {
	select {
	case s.uploads <- struct{}{}:
		return func() { <-s.uploads }, true
	default:
		w.Header().Set("Retry-After", "1")
		failure(w, http.StatusTooManyRequests, fail(ErrBusy, "too many uploads at once: try again"))
		return nil, false
	}
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength < 0 || r.ContentLength > maxBody { // attachments: a slot; a plain message needs none
		release, ok := s.upload(w)
		if !ok {
			return
		}
		defer release()
	}
	var in startIn
	if !readJSONLimit(w, r, &in, maxRunBody) {
		return
	}
	msg, err := in.message()
	if err != nil {
		failure(w, status(err), err)
		return
	}
	snap, err := s.StartMessage(in.Profile, in.Conversation, msg)
	if err != nil {
		failure(w, status(err), err)
		return
	}
	w.Header().Set("Location", "/v1/runs/"+snap.ID)
	respond(w, http.StatusCreated, snap)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	snap, err := s.Get(r.PathValue("id"))
	if err != nil {
		failure(w, status(err), err)
		return
	}
	respond(w, http.StatusOK, snap)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if err := s.Cancel(r.PathValue("id")); err != nil {
		failure(w, status(err), err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	if err := s.Forget(r.PathValue("id")); err != nil {
		failure(w, status(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type decideIn struct {
	Allow *bool `json:"allow"`
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var in decideIn
	if !readJSON(w, r, &in) {
		return
	}
	if in.Allow == nil {
		failure(w, http.StatusBadRequest, fail(ErrInput, `"allow" is required`))
		return
	}
	if err := s.Decide(r.PathValue("id"), r.PathValue("approval"), *in.Allow); err != nil {
		failure(w, status(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// events streams a run's events until it ends and every event was sent,
// or the reader goes.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	j, err := s.job(r.PathValue("id"))
	if err != nil {
		failure(w, status(err), err)
		return
	}
	after, ok := resumeFrom(r)
	if !ok {
		failure(w, http.StatusBadRequest, fail(ErrInput, "bad Last-Event-ID or after"))
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	stream(r.Context(), w, rc, j, after)
}

func stream(ctx context.Context, w io.Writer, rc *http.ResponseController, j *job, after int) {
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		entries, changed, missed, over := j.since(after)
		if !send(w, rc, entries, missed) || over {
			return
		}
		if len(entries) > 0 {
			after = entries[len(entries)-1].id
		}
		if !wait(ctx, w, rc, changed, tick.C) {
			return
		}
	}
}

// wait blocks until the log changes, sending a heartbeat meanwhile;
// false when the reader is gone.
func wait(ctx context.Context, w io.Writer, rc *http.ResponseController, changed <-chan struct{}, tick <-chan time.Time) bool {
	for {
		select {
		case <-changed:
			return true
		case <-ctx.Done():
			return false
		case <-tick:
			if !write(w, rc, []byte(": alive\n\n")) {
				return false
			}
		}
	}
}

// send writes entries as SSE; a gap is announced first.
func send(w io.Writer, rc *http.ResponseController, entries []entry, missed int) bool {
	var b bytes.Buffer
	if missed > 0 {
		b.WriteString("event: dropped\ndata: {\"missed\":")
		b.WriteString(strconv.Itoa(missed))
		b.WriteString("}\n\n")
	}
	for _, e := range entries {
		b.WriteString("id: ")
		b.WriteString(strconv.Itoa(e.id))
		b.WriteString("\ndata: ")
		b.Write(e.data) // one line: JSON without raw newlines
		b.WriteString("\n\n")
	}
	if b.Len() == 0 {
		return true
	}
	return write(w, rc, b.Bytes())
}

// write sends data within a deadline of its own: a reader that stopped
// reading is let go, an idle stream is not.
func write(w io.Writer, rc *http.ResponseController, data []byte) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := w.Write(data); err != nil {
		return false
	}
	return rc.Flush() == nil
}

func resumeFrom(r *http.Request) (int, bool) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("after")
	}
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= 0
}

// readJSON decodes one JSON object of a known shape.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return readJSONLimit(w, r, v, maxBody)
}

// readJSONLimit is readJSON with a body of at most limit bytes.
func readJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		failure(w, http.StatusUnsupportedMediaType, fail(ErrInput, "send application/json"))
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil || !utf8.Valid(data) {
		failure(w, http.StatusBadRequest, fail(ErrInput, "the body is too large or not UTF-8"))
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		failure(w, http.StatusBadRequest, fail(ErrInput, "the body is not one JSON object of the expected fields"))
		return false
	}
	return true
}

// respond encodes v first: a failure becomes a 500, not a 200 with
// nothing after it.
func respond(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		code, data = http.StatusInternalServerError, []byte(`{"error":{"code":"SERVE_ENCODE","message":"the answer could not be encoded"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(data, '\n'))
}

// failure answers with the error's code and its message for the user.
func failure(w http.ResponseWriter, code int, err error) {
	respond(w, code, struct {
		Error *ErrorInfo `json:"error"`
	}{errorInfo(err)})
}

func status(err error) int {
	if code, ok := storeStatus(err); ok {
		return code
	}
	switch {
	case errors.HasCode(err, ErrNotFound):
		return http.StatusNotFound
	case errors.HasCode(err, ErrBusy):
		return http.StatusConflict
	case errors.HasCode(err, ErrClosed):
		return http.StatusServiceUnavailable
	case errors.HasCode(err, ErrInput):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// Serve answers the API on l, which must listen on a loopback address,
// until ctx ends or Close; then it closes l.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		return fail(ErrConfig, "the API listens on a loopback address only")
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10}
	stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
	defer stop()
	stopSrv := context.AfterFunc(s.ctx, func() { _ = srv.Close() })
	defer stopSrv()
	if err := srv.Serve(l); err != nil && !stderrors.Is(err, http.ErrServerClosed) {
		return errors.Wrap(err, ErrConfig, "the API stopped")
	}
	return nil
}
