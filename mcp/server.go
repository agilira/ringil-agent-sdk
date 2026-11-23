// server.go: how to reach an MCP server
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/agilira/go-errors"
	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agilira/ringil-agent-sdk/egress"
	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
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
	// Mounts and reaching only what Proxy allows. Without it the server
	// runs as a plain process with the host's privileges, as coding agents
	// run the servers their user configured.
	Sandbox   sandbox.Sandbox
	Mounts    []sandbox.Mount
	Limits    sandbox.Limits
	Protected []string
	// Proxy is the egress proxy the sandboxed server reaches the network
	// through: its allowed domains, never loopback, the local network or
	// cloud metadata. Nil, no network.
	Proxy *egress.Proxy
	// Refused, when set, hears each connection the proxy refused (host
	// and reason), for telling the user.
	Refused egress.Refused

	// URL of a remote server (Streamable HTTP). HTTPS unless the host is
	// a loopback address, or AllowHTTP is set.
	URL       string
	AllowHTTP bool
	// Headers returns headers for every request, such as an API key from
	// the host's secrets; called per request, so rotated keys apply.
	Headers safehttp.Headers
	// OAuth runs the authorization flow when the server asks for it
	// (auth.NewAuthorizationCodeHandler of the SDK, or the host's own).
	OAuth auth.OAuthHandler
	// HTTPClient is the base client; its redirects are restricted to the
	// URL's origin.
	HTTPClient *http.Client

	// Tools, when set, offers only these tools of the server.
	Tools []string
	// TrustAnnotations takes the server's read-only and destructive hints
	// at their word. Off by default: the hints come from the server, so
	// every tool counts as destructive and an approval hook asks first.
	TrustAnnotations bool
	// MaxOutput is how much of a result the model sees; default 256 KiB.
	MaxOutput int
	// CallTimeout bounds one tool call; default 5 minutes.
	CallTimeout time.Duration
	// MaxResponse bounds one HTTP response body, besides event streams;
	// default 64 MiB.
	MaxResponse int64
}

func (s Server) maxOutput() int {
	if s.MaxOutput > 0 {
		return s.MaxOutput
	}
	return defaultOutput
}

func (s Server) callTimeout() time.Duration {
	if s.CallTimeout > 0 {
		return s.CallTimeout
	}
	return defaultCallTime
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
	case s.Sandbox == nil && (s.Proxy != nil || len(s.Mounts) > 0):
		return errors.New(ErrConfig, "MCP server "+s.Name+": mounts and a proxy are the sandbox's: give it one")
	}
	return nil
}

func (s Server) validateURL() error {
	if err := safehttp.CheckURL(s.URL, s.AllowHTTP); err != nil {
		return errors.Wrap(err, ErrConfig, "MCP server "+s.Name+": "+err.Error())
	}
	return nil
}

func (s Server) transport(ctx context.Context, w *refusals) (sdk.Transport, func() error, error) {
	switch {
	case s.URL != "":
		client := s.httpClient()
		client.Transport = w.watch(client.Transport)
		return &sdk.StreamableClientTransport{Endpoint: s.URL, HTTPClient: client, OAuthHandler: s.OAuth}, nil, nil
	case s.Sandbox != nil:
		return s.sandboxed(ctx)
	}
	// Not CommandContext: Connect's context bounds the start only; the
	// server lives until Close.
	cmd := exec.Command(s.Command[0], s.Command[1:]...) // #nosec G204 -- the host chose the command
	cmd.Env = envList(s.Env)
	ownGroup(cmd)
	return &sdk.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second}, func() error { return killGroup(cmd) }, nil
}

// baseEnv are the host variables a plain local server inherits: enough
// to find interpreters and caches (npx, uvx), no credentials. Env adds
// the rest and wins.
var baseEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "APPDATA", "LOCALAPPDATA", "USERPROFILE"}

func envList(env map[string]string) []string {
	list := make([]string, 0, len(env)+len(baseEnv))
	for _, k := range baseEnv {
		if v, ok := os.LookupEnv(k); ok {
			if _, set := env[k]; !set {
				list = append(list, k+"="+v)
			}
		}
	}
	for k, v := range env {
		list = append(list, k+"="+v)
	}
	return list
}

// sandboxed starts the server in the sandbox and talks to it over pipes.
// The server lives until Close, not until ctx ends: Connect's context
// bounds only the start.
func (s Server) sandboxed(context.Context) (sdk.Transport, func() error, error) {
	box := s.Sandbox
	// The server's input is an OS pipe, not an io.Pipe: with a reader, Go
	// copies it in a goroutine that Wait waits for, so a server that ended
	// at once was seen only at the deadline (23/11).
	toServer, serverIn, err := os.Pipe()
	if err != nil {
		return nil, nil, errors.Wrap(err, ErrConnect, "cannot make the pipe of MCP server "+s.Name)
	}
	serverOut, fromServer := io.Pipe()
	life, cancel := context.WithCancel(context.Background())
	spec := sandbox.Spec{Argv: s.Command, Env: s.Env, Mounts: s.Mounts, Limits: s.Limits, Protected: s.Protected,
		Stdin: toServer, Stdout: fromServer, Stderr: io.Discard}
	unlisten := func() {}
	if s.Proxy != nil {
		sock, stopProxy, err := s.Proxy.Listen(life, s.Refused)
		if err != nil {
			cancel()
			_, _ = toServer.Close(), serverIn.Close() // #nosec G104 -- nothing started
			return nil, nil, errors.Wrap(err, ErrConnect, "cannot open the proxy of MCP server "+s.Name+": "+err.Error())
		}
		spec.Network, spec.Proxy, unlisten = sandbox.ProxiedNetwork, sock, stopProxy
	}
	proc, err := box.Start(life, spec)
	_ = toServer.Close() // #nosec G104 -- the server holds its own copy now
	if err != nil {
		cancel()
		unlisten()           // its socket's directory goes too
		_ = serverIn.Close() // #nosec G104 -- the start failed: nothing reads it
		return nil, nil, errors.Wrap(err, ErrConnect, "cannot start MCP server "+s.Name+" in the sandbox: "+err.Error())
	}
	done := make(chan struct{})
	go func() {
		werr := proc.Wait()
		fromServer.CloseWithError(errOrEOF(werr))
		close(done)
	}()
	stop := func() error {
		cerr := serverIn.Close() // end of input: a well-behaved server exits
		if stderrors.Is(cerr, os.ErrClosed) {
			cerr = nil // the transport closed it first
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		cancel()
		<-done
		unlisten()
		return cerr
	}
	return &boxTransport{IOTransport: &sdk.IOTransport{Reader: serverOut, Writer: serverIn}, done: done}, stop, nil
}

// boxTransport is the sandboxed server's pipes, and the end of its process.
type boxTransport struct {
	*sdk.IOTransport
	done chan struct{}
}

func (b *boxTransport) exited() <-chan struct{} { return b.done }

func errOrEOF(err error) error {
	if err == nil {
		return io.EOF
	}
	return err
}

// httpClient adds the host's headers, keeps redirects on the same origin
// so credentials never follow one elsewhere, and bounds responses.
func (s Server) httpClient() *http.Client {
	return safehttp.Client(s.HTTPClient, s.Headers, s.maxResponse(), true) // MCP streams events
}

// refusals remembers a remote server's 401 or 403, so that a refused key
// is told apart from a server that cannot be reached: the SDK reports
// both as a failed initialize. A later success clears it (audit 22/11: a
// 401 followed by a 500 was told as refused).
type refusals struct {
	next   http.RoundTripper
	status atomic.Int32
}

func (w *refusals) watch(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	w.next = next
	return w
}

func (w *refusals) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := w.next.RoundTrip(req)
	switch {
	case err != nil:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		w.status.Store(int32(resp.StatusCode)) // #nosec G115 -- 401 or 403
	case resp.StatusCode < 300: // the last answer counts: accepted since
		w.status.Store(0)
	}
	return resp, err
}

func (s Server) maxResponse() int64 {
	if s.MaxResponse > 0 {
		return s.MaxResponse
	}
	return 64 << 20
}
