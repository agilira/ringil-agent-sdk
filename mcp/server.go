// server.go: how to reach an MCP server
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"time"

	"github.com/agilira/go-errors"
	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agilira/ringil-agent-sdk/sandbox"
)

// Server describes one MCP server: a Command for a local one, or a URL
// for a remote one.
type Server struct {
	// Name prefixes the server's tools; letters, digits, _ and -.
	Name string

	// Command and its arguments start a local server over stdio. It
	// never passes through a shell.
	Command []string
	// Env is the local server's whole environment (in the sandbox,
	// besides PATH, HOME, TMPDIR and LANG).
	Env map[string]string
	// Sandbox, when set, runs the local server isolated, seeing only
	// Mounts and, if Network allows, the network. Without it the server
	// runs as a plain process with the host's privileges.
	Sandbox   sandbox.Sandbox
	Mounts    []sandbox.Mount
	Network   sandbox.Network
	Limits    sandbox.Limits
	Protected []string

	// URL of a remote server (Streamable HTTP). HTTPS unless the host is
	// a loopback address, or AllowHTTP is set.
	URL       string
	AllowHTTP bool
	// Headers returns headers for every request, such as an API key from
	// the host's secrets; called per request, so rotated keys apply.
	Headers func(ctx context.Context) (http.Header, error)
	// OAuth runs the authorization flow when the server asks for it
	// (auth.NewAuthorizationCodeHandler of the SDK, or the host's own).
	OAuth auth.OAuthHandler
	// HTTPClient is the base client; its redirects are restricted to the
	// URL's origin.
	HTTPClient *http.Client

	// Tools, when set, offers only these tools of the server.
	Tools []string
}

var serverName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

func (s Server) validate() error {
	switch {
	case !serverName.MatchString(s.Name):
		return errors.New(ErrConfig, "MCP server names are 1-32 letters, digits, _ or -")
	case (len(s.Command) == 0) == (s.URL == ""):
		return errors.New(ErrConfig, "MCP server "+s.Name+" needs either a command or a URL")
	case s.URL != "":
		return s.validateURL()
	}
	return nil
}

func (s Server) validateURL() error {
	u, err := url.Parse(s.URL)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New(ErrConfig, "MCP server "+s.Name+" has an invalid URL (credentials and fragments are not allowed)")
	}
	if u.Scheme == "https" || (u.Scheme == "http" && (s.AllowHTTP || loopback(u.Hostname()))) {
		return nil
	}
	return errors.New(ErrConfig, "MCP server "+s.Name+" must use https")
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s Server) transport(ctx context.Context) (sdk.Transport, func() error, error) {
	switch {
	case s.URL != "":
		return &sdk.StreamableClientTransport{Endpoint: s.URL, HTTPClient: s.httpClient(), OAuthHandler: s.OAuth}, nil, nil
	case s.Sandbox != nil:
		return s.sandboxed(ctx)
	}
	// Not CommandContext: Connect's context bounds the start only; the
	// server lives until Close.
	cmd := exec.Command(s.Command[0], s.Command[1:]...) // #nosec G204 -- the host chose the command

	cmd.Env = envList(s.Env)
	return &sdk.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second}, nil, nil
}

func envList(env map[string]string) []string {
	list := make([]string, 0, len(env))
	for k, v := range env {
		list = append(list, k+"="+v)
	}
	return list
}

// sandboxed starts the server in the sandbox and talks to it over pipes.
// The server lives until Close, not until ctx ends: Connect's context
// bounds only the start.
func (s Server) sandboxed(context.Context) (sdk.Transport, func() error, error) {
	toServer, serverIn := io.Pipe()
	serverOut, fromServer := io.Pipe()
	life, cancel := context.WithCancel(context.Background())
	proc, err := s.Sandbox.Start(life, sandbox.Spec{
		Argv: s.Command, Env: s.Env, Mounts: s.Mounts, Network: s.Network, Limits: s.Limits, Protected: s.Protected,
		Stdin: toServer, Stdout: fromServer, Stderr: io.Discard,
	})
	if err != nil {
		cancel()
		return nil, nil, errors.Wrap(err, ErrConnect, "cannot start MCP server "+s.Name+" in the sandbox: "+err.Error())
	}
	done := make(chan struct{})
	go func() {
		werr := proc.Wait()
		fromServer.CloseWithError(errOrEOF(werr))
		close(done)
	}()
	stop := func() error {
		cerr := serverIn.Close()
		cancel()
		<-done
		return cerr
	}
	return &sdk.IOTransport{Reader: serverOut, Writer: serverIn}, stop, nil
}

func errOrEOF(err error) error {
	if err == nil {
		return io.EOF
	}
	return err
}

// httpClient adds the host's headers and keeps redirects on the same
// origin, so credentials never follow a redirect elsewhere.
func (s Server) httpClient() *http.Client {
	base := s.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	c := *base
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c.Transport = headerTransport{next: next, headers: s.Headers}
	c.CheckRedirect = sameOrigin
	return &c
}

func sameOrigin(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
		return http.ErrUseLastResponse
	}
	return nil
}

type headerTransport struct {
	next    http.RoundTripper
	headers func(ctx context.Context) (http.Header, error)
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.headers == nil {
		return t.next.RoundTrip(req)
	}
	h, err := t.headers(req.Context())
	if err != nil {
		return nil, errors.Wrap(err, ErrConnect, "cannot get the MCP server's credentials")
	}
	req = req.Clone(req.Context())
	for k, vs := range h {
		req.Header[http.CanonicalHeaderKey(k)] = vs
	}
	return t.next.RoundTrip(req)
}
