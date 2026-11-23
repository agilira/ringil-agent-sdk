// safehttp.go: HTTP for remote servers that receive the user's credentials
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package safehttp holds the rules shared by the clients of remote agent
// servers (MCP, A2A): HTTPS unless loopback, credentials added on every
// request and never sent to another origin, response bodies bounded.
package safehttp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/agilira/go-errors"
)

// Error codes.
const (
	ErrURL         errors.ErrorCode = "HTTP_URL"
	ErrCredentials errors.ErrorCode = "HTTP_CREDENTIALS"
	ErrTooLarge    errors.ErrorCode = "HTTP_TOO_LARGE"
)

// Headers returns the headers of one request, such as an API key read
// from the host's secrets at that moment.
type Headers func(ctx context.Context) (http.Header, error)

// CheckURL accepts an https URL, or http to a loopback address (or any
// host when allowHTTP), without credentials or a fragment.
func CheckURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New(ErrURL, "invalid URL (credentials and fragments are not allowed)")
	}
	if u.Scheme == "https" || (u.Scheme == "http" && (allowHTTP || Loopback(u.Hostname()))) {
		return nil
	}
	return errors.New(ErrURL, "the URL must use https")
}

// Loopback reports a literal loopback address.
func Loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SameOrigin reports whether two URLs share scheme and host:port.
func SameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Scheme == ub.Scheme && strings.EqualFold(ua.Host, ub.Host)
}

// Client returns a copy of base (or a new client) that adds headers to
// every request, follows redirects only on the same origin, and fails a
// response body larger than max bytes. With streams, bodies labelled
// text/event-stream are exempt: a long-lived stream of events is bounded
// per event by its reader, not in total. A client that never streams
// must not set it, or any response could escape the bound by its label.
func Client(base *http.Client, headers Headers, max int64, streams bool) *http.Client {
	if base == nil {
		base = &http.Client{}
	}
	c := *base
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	if t, ok := next.(*http.Transport); ok {
		next = tunnelOnly(t)
	}
	c.Transport = transport{next: next, headers: headers, max: max, streams: streams}
	c.CheckRedirect = sameOriginRedirect
	return &c
}

// tunnelOnly is t with its proxy (the environment's, by default) used
// only for https, which the proxy tunnels without reading: a plain http
// request through it would hand the proxy the credentials in clear, and
// let it answer in the server's place (audit 21/11, a Vault token).
func tunnelOnly(t *http.Transport) *http.Transport {
	proxy := t.Proxy
	if proxy == nil {
		return t
	}
	c := t.Clone()
	c.Proxy = func(req *http.Request) (*url.URL, error) {
		if req.URL.Scheme != "https" {
			return nil, nil
		}
		return proxy(req)
	}
	return c
}

func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || req.URL.Scheme != via[0].URL.Scheme || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return http.ErrUseLastResponse
	}
	return nil
}

type transport struct {
	next    http.RoundTripper
	headers Headers
	max     int64
	streams bool
}

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.headers != nil {
		h, err := t.headers(req.Context())
		if err != nil {
			// The wrapped err stays in the chain; its text is not repeated in
			// the message, which a host headers-func could fill with a
			// credential path or value that then reaches logs and the model.
			return nil, errors.Wrap(err, ErrCredentials, "cannot get the server's credentials")
		}
		req = req.Clone(req.Context())
		for k, vs := range h {
			req.Header[http.CanonicalHeaderKey(k)] = vs
		}
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	stream := t.streams && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	if t.max > 0 && !stream {
		resp.Body = &limitedBody{rc: resp.Body, max: t.max}
	}
	return resp, err
}

// limitedBody fails a response body that grows past its bound, instead of
// letting a server fill the client's memory. A body of exactly max bytes is
// allowed whether its EOF arrives with the last bytes or in a read of its
// own: the bound is decided on the count delivered, not on a read reaching
// an exhausted budget.
type limitedBody struct {
	rc  io.ReadCloser
	n   int64 // bytes delivered so far
	max int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.n > b.max {
		return 0, errors.New(ErrTooLarge, "the server's response is too large")
	}
	// Read at most one byte past max: enough to tell a max-sized body from a
	// larger one, without buffering the whole overflow (overflow-safe near MaxInt64).
	if left := b.max - b.n; int64(len(p))-1 > left {
		p = p[:left+1]
	}
	m, err := b.rc.Read(p)
	b.n += int64(m)
	if b.n > b.max {
		return 0, errors.New(ErrTooLarge, "the server's response is too large")
	}
	return m, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
