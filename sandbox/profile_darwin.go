// profile_darwin.go: the Seatbelt profile and the proxy's loopback bridge
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build darwin

package sandbox

import (
	stderrors "errors"
	"io"
	"net"
	"path"
	"strings"
	"sync"

	"github.com/agilira/go-errors"
)

// systemRead are what every program needs to read: the system, its
// libraries and frameworks, configuration, the developer tools. Real
// paths: /etc and /var are links into /private.
var systemRead = []string{
	"/usr", "/bin", "/sbin", "/System", "/Library", "/opt", "/dev",
	"/private/etc", "/private/var/db", "/private/var/select",
}

// developerTools matches every Xcode, however named (Xcode_26.6.app,
// Xcode-beta.app): /usr/bin's python3, git and cc are shims that run the
// active one.
const developerTools = `(regex #"^/Applications/Xcode[^/]*\.app(/|$)")`

// machServices are the system services a sandboxed program may talk to:
// logging, user and group lookup, preferences, temporary directories and
// certificate checks (HTTPS through the proxy).
var machServices = []string{
	"com.apple.system.logger", "com.apple.system.notification_center", "com.apple.logd", "com.apple.diagnosticd",
	"com.apple.system.opendirectoryd.libinfo", "com.apple.system.opendirectoryd.membership",
	"com.apple.cfprefsd.daemon", "com.apple.cfprefsd.agent", "com.apple.bsd.dirhelper",
	"com.apple.trustd", "com.apple.trustd.agent", "com.apple.SecurityServer", "com.apple.ocspd",
}

// quote writes s as a profile string.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// buildProfile denies everything, then allows what the spec grants. Later
// rules win, so the denials of hidden and read-only paths come last.
func buildProfile(spec Spec, grants []grant, tmp, proxy string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("(allow process-exec process-fork)\n(allow signal (target same-sandbox))\n(allow sysctl-read)\n")
	b.WriteString("(allow ipc-posix-shm-read-data ipc-posix-shm-write-data ipc-posix-shm-write-create ipc-posix-shm-read-metadata)\n")
	for _, s := range machServices {
		b.WriteString("(allow mach-lookup (global-name " + quote(s) + "))\n")
	}
	// Seeing that a path exists (stat) is allowed anywhere: resolving
	// paths needs it. Reading what is in a file or a directory is not.
	b.WriteString("(allow file-read-metadata)\n(allow file-read* (literal \"/\"))\n")
	for _, p := range systemRead {
		b.WriteString("(allow file-read* (subpath " + quote(p) + "))\n")
	}
	b.WriteString("(allow file-read* " + developerTools + ")\n")
	b.WriteString("(allow file-read* file-write* (subpath " + quote(tmp) + "))\n")
	b.WriteString("(allow file-write* (literal \"/dev/null\") (literal \"/dev/zero\") (literal \"/dev/tty\") (regex #\"^/dev/fd/\"))\n")
	b.WriteString("(allow file-ioctl (literal \"/dev/tty\") (regex #\"^/dev/fd/\"))\n")
	for _, g := range grants {
		b.WriteString(grantRules(g))
	}
	b.WriteString(networkRules(spec.Network, proxy))
	for _, g := range grants {
		b.WriteString(denyRules(g))
	}
	return b.String()
}

func grantRules(g grant) string {
	filter := "(literal " + quote(g.real) + ")"
	if g.dir {
		filter = "(subpath " + quote(g.real) + ")"
	}
	rule := "(allow file-read* " + filter + ")\n"
	if g.Writable {
		rule += "(allow file-write* " + filter + ")\n"
	}
	return rule
}

// denyRules keeps a grant's read-only paths unwritable and its hidden
// paths unreadable, and pins the directories above both: a rule follows
// a path, so renaming a parent would carry a path out of it. Hard links
// out of either are refused too: a link elsewhere is the same file.
func denyRules(g grant) string {
	var b strings.Builder
	pins := map[string]bool{}
	pin := func(rel string) {
		for d := path.Dir(rel); d != "." && !pins[d]; d = path.Dir(d) {
			pins[d] = true
			b.WriteString("(deny file-write-unlink file-write-mode file-write-owner (literal " + quote(path.Join(g.real, d)) + "))\n")
		}
	}
	for _, rel := range g.ReadOnly {
		p := quote(path.Join(g.real, rel))
		b.WriteString("(deny file-write* file-link (subpath " + p + "))\n")
		pin(rel)
	}
	for _, rel := range g.Hide {
		p := quote(path.Join(g.real, rel))
		b.WriteString("(deny file-read-data file-write* file-link (subpath " + p + "))\n")
		pin(rel)
	}
	return b.String()
}

// networkRules: none, only the proxy's loopback port, or anywhere but
// the host's loopback (DNS through the system's resolver).
func networkRules(n Network, proxy string) string {
	switch n {
	case ProxiedNetwork:
		_, port, _ := net.SplitHostPort(proxy)
		return "(allow network-outbound (remote ip " + quote("localhost:"+port) + "))\n"
	case IsolatedNetwork:
		return "(allow network-outbound (remote ip \"*:*\"))\n" +
			"(deny network-outbound (remote ip \"localhost:*\"))\n" +
			"(allow network-outbound (literal \"/private/var/run/mDNSResponder\"))\n" +
			"(allow mach-lookup (global-name \"com.apple.dnssd.service\"))\n"
	}
	return ""
}

// loopBridge listens on a loopback port and carries each connection to
// the proxy's Unix socket: Seatbelt can allow one TCP port, and clients
// speak to proxies over TCP.
type loopBridge struct {
	l    net.Listener
	sock string
	wg   sync.WaitGroup
	mu   sync.Mutex
	open map[net.Conn]bool
}

func newLoopBridge(sock string) (*loopBridge, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.Wrap(err, ErrStart, "cannot listen for the proxy")
	}
	b := &loopBridge{l: l, sock: sock, open: map[net.Conn]bool{}}
	b.wg.Go(b.serve)
	return b, nil
}

func (b *loopBridge) addr() string { return b.l.Addr().String() }

func (b *loopBridge) serve() {
	for {
		c, err := b.l.Accept()
		if err != nil {
			return
		}
		b.wg.Go(func() { b.carry(c) })
	}
}

func (b *loopBridge) track(c net.Conn, on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if on {
		b.open[c] = true
	} else {
		delete(b.open, c)
	}
}

func (b *loopBridge) carry(c net.Conn) {
	defer func() { _ = c.Close() }() // #nosec G104 -- done
	up, err := net.Dial("unix", b.sock)
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }() // #nosec G104 -- done
	b.track(c, true)
	b.track(up, true)
	defer b.track(c, false)
	defer b.track(up, false)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }() // #nosec G104 -- either side may end
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }() // #nosec G104 -- as above
	<-done
}

// close stops listening and ends every carried connection.
func (b *loopBridge) close() error {
	err := b.l.Close()
	b.mu.Lock()
	for c := range b.open {
		_ = c.Close() // #nosec G104 -- ending it
	}
	b.mu.Unlock()
	b.wg.Wait()
	if err != nil && !stderrors.Is(err, net.ErrClosed) {
		return errors.Wrap(err, ErrStart, "cannot close the proxy bridge")
	}
	return nil
}
