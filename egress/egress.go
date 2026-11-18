// egress.go: the proxy that decides where sandboxed programs connect
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package egress is an HTTP proxy that lets a sandboxed program reach the
// domains allowed to it and nothing else. A program under the sandbox's
// ProxiedNetwork has no other way out: its HTTP_PROXY points here.
//
// A domain is allowed by the configuration or, when Ask is set, by the
// user the first time it is wanted; the answer holds for the proxy's life.
// Whatever the domain, the proxy never connects to the host itself, the
// local network or cloud metadata: names are resolved here and every
// address is checked before dialling, so a name that resolves (or
// rebinds) to 127.0.0.1 or 169.254.169.254 is refused. Only the allowed
// ports are reached, 80 and 443 by default.
//
// HTTPS goes through CONNECT and stays encrypted end to end: the proxy
// sees the name and the port, never the content. Plain HTTP is forwarded
// one request per connection.
package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"
)

// Error codes.
const (
	// ErrConfig: an allowed domain or port is malformed.
	ErrConfig errors.ErrorCode = "EGRESS_CONFIG"
	// ErrListen: the proxy's socket could not be made.
	ErrListen errors.ErrorCode = "EGRESS_LISTEN"
)

// Timeouts and bounds.
const (
	headerTimeout = 30 * time.Second
	dialTimeout   = 15 * time.Second
	// MaxConns bounds the connections one Serve carries at once.
	MaxConns = 64
)

// Config says what the proxy allows.
type Config struct {
	// Allow are the domains reachable without asking: "github.com" is that
	// name only, "*.github.com" its subdomains (not github.com itself).
	Allow []string
	// Ask decides a domain that is not allowed; nil refuses it. The answer
	// is kept for the proxy's life: a domain is asked about once.
	Ask func(ctx context.Context, host string) (bool, error)
	// Ports are the ports reachable; default 80 and 443.
	Ports []int
	// Private lets connections go to loopback, private, link-local and
	// other non-public addresses. Off, they are refused whatever the name.
	Private bool
	// Resolver resolves names; default net.DefaultResolver.
	Resolver interface {
		LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	}
	// Dial connects to a checked address; default a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Proxy decides and carries connections. Its decisions are shared by every
// Serve.
type Proxy struct {
	cfg      Config
	exact    map[string]bool
	suffixes []string
	ports    map[int]bool

	mu      sync.Mutex
	decided map[string]bool
	asking  chan struct{} // one question at a time; waiting ends with the context
}

// New checks the configuration and returns the proxy.
func New(cfg Config) (*Proxy, error) {
	p := &Proxy{cfg: cfg, exact: map[string]bool{}, ports: map[int]bool{}, decided: map[string]bool{}, asking: make(chan struct{}, 1)}
	for _, d := range cfg.Allow {
		if err := p.allow(d); err != nil {
			return nil, err
		}
	}
	if len(cfg.Ports) == 0 {
		cfg.Ports = []int{80, 443}
	}
	for _, port := range cfg.Ports {
		if port < 1 || port > 65535 {
			return nil, errors.New(ErrConfig, "ports are 1-65535: "+strconv.Itoa(port))
		}
		p.ports[port] = true
	}
	if p.cfg.Resolver == nil {
		p.cfg.Resolver = net.DefaultResolver
	}
	if p.cfg.Dial == nil {
		d := &net.Dialer{Timeout: dialTimeout}
		p.cfg.Dial = d.DialContext
	}
	return p, nil
}

func (p *Proxy) allow(domain string) error {
	name, wild := strings.CutPrefix(domain, "*.")
	name = normal(name)
	if !validName(name) || !strings.Contains(name, ".") && wild {
		return errors.New(ErrConfig, "allowed domains are names such as example.com or *.example.com: "+domain)
	}
	if wild {
		p.suffixes = append(p.suffixes, "."+name)
	} else {
		p.exact[name] = true
	}
	return nil
}

// Allowed reports whether host may be reached, asking when configured.
func (p *Proxy) Allowed(ctx context.Context, host string) (bool, error) {
	host = normal(host)
	if !validName(host) && !isIP(host) {
		return false, nil
	}
	if p.listed(host) {
		return true, nil
	}
	if p.cfg.Ask == nil {
		return false, nil
	}
	if ok, known := p.known(host); known {
		return ok, nil
	}
	return p.ask(ctx, host)
}

// ask puts one question at a time to the user; waiting ends with ctx.
func (p *Proxy) ask(ctx context.Context, host string) (bool, error) {
	select {
	case p.asking <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-p.asking }()
	if ok, known := p.known(host); known {
		return ok, nil // decided while this one waited
	}
	ok, err := p.cfg.Ask(ctx, host)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	p.decided[host] = ok
	p.mu.Unlock()
	return ok, nil
}

func (p *Proxy) listed(host string) bool {
	if p.exact[host] {
		return true
	}
	for _, s := range p.suffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

func (p *Proxy) known(host string) (bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ok, known := p.decided[host]
	return ok, known
}

// normal lower-cases a name and drops a final dot.
func normal(host string) string { return strings.TrimSuffix(strings.ToLower(host), ".") }

// validName accepts ASCII host names: letters, digits and hyphens in
// labels of 1-63, 253 in all. Anything else (Unicode look-alikes,
// underscores, empty labels) is refused rather than interpreted.
func validName(name string) bool {
	if name == "" || len(name) > 253 || isIP(name) {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if !validLabel(label) {
			return false
		}
	}
	return true
}

func validLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	return strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-") == ""
}

func isIP(host string) bool {
	_, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil
}

// Refused hears of each connection the proxy refuses: the host and why,
// as the program was told. Over HTTPS a client shows only a status, so
// the host relays these to whoever ran the program.
type Refused func(host, why string)

// Listen serves the proxy on a new Unix socket in a private directory
// until ctx ends, and returns the socket's path. Questions about domains
// are asked with ctx; refusals go to refused, when set. stop closes the
// socket and every connection; it is also what ctx ending does.
func (p *Proxy) Listen(ctx context.Context, refused Refused) (path string, stop func(), err error) {
	dir, err := os.MkdirTemp("", "ringil-egress-")
	if err != nil {
		return "", nil, errors.Wrap(err, ErrListen, "cannot make the proxy's directory")
	}
	path = filepath.Join(dir, "proxy.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir) // #nosec G104 -- best effort; the listen error is the one to report
		return "", nil, errors.Wrap(err, ErrListen, "cannot listen on the proxy's socket")
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Serve(ctx, l, refused)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
			_ = os.RemoveAll(dir) // #nosec G104 -- a temporary directory; nothing to recover
		})
	}
	return path, stop, nil
}

// Serve carries connections from l until ctx ends or l fails, then closes
// l and every connection it carried.
func (p *Proxy) Serve(ctx context.Context, l net.Listener, refused Refused) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, MaxConns)
	stop := context.AfterFunc(ctx, func() { _ = l.Close() }) // #nosec G104 -- closing to stop
	defer stop()
	for {
		c, err := l.Accept()
		if err != nil {
			break
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = c.Close() // #nosec G104 -- over the bound: refused
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			p.handle(ctx, c, refused)
		})
	}
	_ = l.Close() // #nosec G104 -- already failing or closed
	wg.Wait()
}

// handle reads one request and carries it.
func (p *Proxy) handle(ctx context.Context, c net.Conn, refused Refused) {
	defer func() { _ = c.Close() }() // #nosec G104 -- the connection is done
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	_ = c.SetReadDeadline(time.Now().Add(headerTimeout)) // #nosec G104 -- a failed deadline only loses the bound
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{}) // #nosec G104 -- as above
	host, port, ok := target(req)
	if !ok {
		refuse(c, http.StatusBadRequest, "only CONNECT and absolute http:// requests are proxied")
		return
	}
	up, status, why := p.open(ctx, host, port)
	if up == nil {
		if refused != nil {
			refused(host, why)
		}
		refuse(c, status, why)
		return
	}
	defer func() { _ = up.Close() }() // #nosec G104 -- the tunnel is done
	if req.Method == http.MethodConnect {
		if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
		pipe(c, br, up)
		return
	}
	forward(c, up, req)
}

// target is the host and port a request wants.
func target(req *http.Request) (string, int, bool) {
	authority := req.Host
	defPort := 443
	if req.Method != http.MethodConnect {
		if req.URL == nil || req.URL.Scheme != "http" || req.URL.Host == "" {
			return "", 0, false
		}
		authority, defPort = req.URL.Host, 80
	}
	host, portText, err := net.SplitHostPort(authority)
	if err != nil {
		host, portText = authority, strconv.Itoa(defPort)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || host == "" {
		return "", 0, false
	}
	return strings.Trim(host, "[]"), port, true
}

// open decides host and port and dials a checked address. On refusal it
// returns the status and the reason the program will read.
func (p *Proxy) open(ctx context.Context, host string, port int) (net.Conn, int, string) {
	if !p.ports[port] {
		return nil, http.StatusForbidden, "port " + strconv.Itoa(port) + " is not allowed"
	}
	if ok, err := p.Allowed(ctx, host); err != nil || !ok {
		if p.cfg.Ask != nil {
			return nil, http.StatusForbidden, "the user did not allow the domain " + host
		}
		return nil, http.StatusForbidden, "the domain " + host + " is not allowed (the user can allow it)"
	}
	addrs, err := p.resolve(ctx, host)
	if err != nil {
		return nil, http.StatusBadGateway, "cannot resolve " + host
	}
	return p.dial(ctx, host, addrs, port)
}

// dial tries the addresses the proxy may reach, in order.
func (p *Proxy) dial(ctx context.Context, host string, addrs []netip.Addr, port int) (net.Conn, int, string) {
	usable := false
	for _, a := range addrs {
		if !p.cfg.Private && !public(a) {
			continue
		}
		usable = true
		conn, err := p.cfg.Dial(ctx, "tcp", netip.AddrPortFrom(a, uint16(port)).String()) // #nosec G115 -- port checked 1-65535
		if err == nil {
			return conn, 0, ""
		}
	}
	if !usable {
		return nil, http.StatusForbidden, host + " resolves to a private or local address"
	}
	return nil, http.StatusBadGateway, "cannot connect to " + host
}

func (p *Proxy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a.Unmap()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	addrs, err := p.cfg.Resolver.LookupNetIP(ctx, "ip", host)
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	return addrs, err
}

// nonPublic are ranges no public service uses: the host, the local
// network, carrier NAT, cloud metadata, benchmarks, documentation,
// reserved space, and IPv6 translations that can embed any of them.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("100::/64"),
}

// public reports whether a is an address on the public internet.
func public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func refuse(c net.Conn, status int, why string) {
	body := "ringil egress: " + why + "\n"
	_, _ = io.WriteString(c, "HTTP/1.1 "+strconv.Itoa(status)+" "+http.StatusText(status)+ // #nosec G104 -- the client may be gone
		"\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: "+strconv.Itoa(len(body))+
		"\r\nConnection: close\r\n\r\n"+body)
}

// pipe carries a tunnel both ways until either side ends.
func pipe(c net.Conn, br *bufio.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(up, br) // #nosec G104 -- the tunnel ends either way
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(c, up) // #nosec G104 -- as above
		closeWrite(c)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if hc, ok := c.(interface{ CloseWrite() error }); ok {
		_ = hc.CloseWrite() // #nosec G104 -- the other side may be gone
	}
}

// hopByHop are headers for this hop only, never forwarded.
var hopByHop = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// forward sends one plain HTTP request upstream and carries the answer
// back; both connections close after it.
func forward(c net.Conn, up net.Conn, req *http.Request) {
	for f := range strings.SplitSeq(req.Header.Get("Connection"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			req.Header.Del(f)
		}
	}
	for _, h := range hopByHop {
		req.Header.Del(h)
	}
	req.Close = true
	req.RequestURI = ""
	if err := req.Write(up); err != nil { // the body streams from the client's reader
		return
	}
	_, _ = io.Copy(c, up) // #nosec G104 -- the client may be gone
}
