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
//	POST   /v1/runs                              {"profile","input","conversation"?} starts a run
//	GET    /v1/runs                              the runs kept
//	GET    /v1/runs/{id}                         a run
//	GET    /v1/runs/{id}/events                  its events, as Server-Sent Events (Last-Event-ID or ?after= to resume)
//	POST   /v1/runs/{id}/cancel                  stop it
//	POST   /v1/runs/{id}/approvals/{approval}    {"allow": true|false} decide a waiting tool call
//	DELETE /v1/runs/{id}                         forget a finished run
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
	s.mu.Lock()
	for pattern, h := range s.mounts {
		mux.Handle(pattern, h)
	}
	s.mu.Unlock()
	return s.Guard(mux)
}

// Guard admits to next only the token's holder, by loopback name, not
// from a browser page: what the API itself requires, for a host mounting
// more on the same listener (an A2A endpoint).
func (s *Server) Guard(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !loopbackHost(r.Host):
			// DNS rebinding: a page whose name now points at 127.0.0.1.
			failure(w, http.StatusForbidden, fail("SERVE_HOST", "requests must name the loopback host"))
		case r.Header.Get("Origin") != "":
			failure(w, http.StatusForbidden, fail("SERVE_ORIGIN", "requests from web pages are refused"))
		case subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1:
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
	Profile      string `json:"profile"`
	Input        string `json:"input"`
	Conversation string `json:"conversation,omitempty"`
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	var in startIn
	if !readJSON(w, r, &in) {
		return
	}
	snap, err := s.Start(in.Profile, in.Conversation, in.Input)
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
		b.WriteString("event: dropped\ndata: {\"missed\":" + strconv.Itoa(missed) + "}\n\n")
	}
	for _, e := range entries {
		b.WriteString("id: " + strconv.Itoa(e.id) + "\ndata: ")
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
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		failure(w, http.StatusUnsupportedMediaType, fail(ErrInput, "send application/json"))
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
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
