// sandbox.go: running untrusted programs in isolation
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package sandbox runs a program isolated from the host: only the
// directories granted to it are visible, read-only unless granted
// writable; it sees no environment it was not given, no host processes,
// and by default no network. Skills' scripts, MCP stdio servers and the
// bash tool all run through it.
//
// The Linux backend uses bubblewrap (namespaces, no capabilities, a
// seccomp filter) with per-process resource limits. Other platforms report
// ErrUnavailable: there is never a silent fallback to running unconfined.
package sandbox

import (
	"context"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/agilira/go-errors"
)

// Error codes.
const (
	// ErrUnavailable: this platform or host cannot provide the sandbox.
	ErrUnavailable errors.ErrorCode = "SANDBOX_UNAVAILABLE"
	// ErrConfig: the spec is invalid.
	ErrConfig errors.ErrorCode = "SANDBOX_CONFIG"
	// ErrGrant: a mount cannot be granted, e.g. it overlaps protected data.
	ErrGrant errors.ErrorCode = "SANDBOX_GRANT"
	// ErrStart: the sandbox could not be started.
	ErrStart errors.ErrorCode = "SANDBOX_START"
)

// Network says what network the sandboxed program gets.
type Network int

const (
	// NoNetwork: a private network namespace with no interfaces, and no
	// sockets at all, so a granted directory holding a host Unix socket
	// cannot be used to reach the host.
	NoNetwork Network = iota
	// IsolatedNetwork: outbound IPv4/IPv6 through a user-mode network stack
	// (slirp4netns). Services the host binds to loopback only are not
	// reachable and Unix sockets are refused. Everything else is: other
	// hosts, the LAN, and the host's own services bound to a non-loopback
	// address (including container and VM bridges such as docker0); DNS
	// goes through the host's resolver, local names included. An egress
	// filter is planned; until then grant network only where that is fine.
	IsolatedNetwork
	// ProxiedNetwork: no network but one address, ProxyAddr, which leads to
	// the host's Unix socket Spec.Proxy: an egress proxy (package egress)
	// deciding which domains the program may reach. HTTP_PROXY and its
	// kin point at it. Nothing else answers: no DNS, no other host, no
	// host service, no Unix socket.
	ProxiedNetwork
)

// ProxyAddr is where a program under ProxiedNetwork finds its proxy on
// Linux; on macOS it is another loopback port, in HTTP_PROXY as always.
const ProxyAddr = "127.0.0.1:3128"

// Mount grants a host directory to the sandbox.
type Mount struct {
	// Source is the host directory.
	Source string
	// Target is the absolute path inside the sandbox; empty means the
	// same path as Source, so host paths the user mentions work as they are.
	Target string
	// Writable lets the program change the directory's contents.
	Writable bool
	// Hide are paths inside Source, relative and slash-separated, that the
	// program must not see: each becomes an empty directory or an empty
	// file. files.Hidden lists what the file tools' policy excludes.
	Hide []string
	// ReadOnly are paths inside a writable Source, relative and
	// slash-separated, that stay read-only: a repository's hooks, say.
	// Paths that do not exist are skipped: the program can create them.
	ReadOnly []string
}

// Limits are per-process resource limits. Zero means the default given.
// They are not a total: many processes, or memory held outside the address
// space (memfd), can add up past them. A cgroup-based total is planned;
// until then bound the run with the context's deadline.
type Limits struct {
	// Memory caps the address space of each process; default 8 GiB.
	// Address space is not RAM: runtimes such as OpenBLAS, V8 and the JVM
	// reserve far more than they use, and fail under a tight cap.
	Memory int64
	// Processes caps the processes and threads of the sandbox user;
	// default 256 (numerical libraries start a thread per core).
	Processes int
	// CPU caps the CPU time of each process; default 60s.
	CPU time.Duration
	// FileSize caps the size of a file a process writes; default 256 MiB.
	FileSize int64
	// OpenFiles caps the open descriptors of each process; default 256.
	OpenFiles int
}

// Spec describes one sandboxed program.
type Spec struct {
	// Argv is the program and its arguments, resolved inside the sandbox
	// (where /usr is the host's, read-only). It never passes through a
	// shell.
	Argv []string
	// Dir is the working directory inside the sandbox; default "/".
	Dir string
	// Env is the whole environment, besides PATH, HOME, TMPDIR, LANG and,
	// under ProxiedNetwork, the proxy variables.
	Env map[string]string
	// Path is PATH, directories inside the sandbox; default /usr/bin, /bin.
	Path []string
	// Home, when set, is HOME: a private, empty, writable directory at
	// this absolute path, gone when the program ends, where mounts may
	// place a user's configuration (a .gitconfig, a toolchain). Unset,
	// HOME is /tmp.
	Home string
	// HomeShown, with no Home, is the user's home where a mount grants it
	// at its own path: HOME points there (and XDG_CACHE_HOME to the
	// temporary directory), so ~ finds the user's files.
	HomeShown string
	Mounts    []Mount
	Network   Network
	// Proxy is the host's Unix socket of the egress proxy, for
	// ProxiedNetwork.
	Proxy  string
	Limits Limits
	// Protected are absolute host paths that no mount may be inside, or
	// contain unless it hides them (in a writable mount they must exist):
	// credentials, session stores, the agent's own configuration.
	// They are compared by resolved path: a hard link or bind-mount alias
	// elsewhere is not detected, so do not grant directories holding such
	// aliases. Mount sources are resolved too, so a source path must not be
	// one the sandboxed program could have rewritten (a symlink planted in
	// an earlier writable grant would redirect it).
	Protected []string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
}

// Process is a started sandboxed program.
type Process interface {
	// Wait waits for the program to exit and releases the sandbox. When the
	// context passed to Start ends, the whole sandbox is killed, including
	// any processes the program started.
	Wait() error
}

// Sandbox starts sandboxed programs.
type Sandbox interface {
	Start(ctx context.Context, spec Spec) (Process, error)
}

// New returns the sandbox of this platform, or ErrUnavailable.
func New() (Sandbox, error) { return newPlatform() }

func configError(msg string) error { return errors.New(ErrConfig, msg) }

// validate checks the parts of a spec that do not depend on the platform.
func validate(spec Spec) error {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return configError("argv is empty")
	}
	if spec.Dir != "" && !path.IsAbs(spec.Dir) {
		return configError("dir must be an absolute path inside the sandbox")
	}
	if err := validateMounts(spec.Mounts); err != nil {
		return err
	}
	if err := validateHome(spec); err != nil {
		return err
	}
	if err := validatePath(spec.Path); err != nil {
		return err
	}
	if err := validateProxy(spec); err != nil {
		return err
	}
	return validateEnv(spec.Env)
}

func validateProxy(spec Spec) error {
	if (spec.Network == ProxiedNetwork) != (spec.Proxy != "") || spec.Proxy != "" && !filepath.IsAbs(spec.Proxy) {
		return configError("a proxied network needs the proxy's absolute socket path, and only it")
	}
	return nil
}

// validateHome: the home is a clean absolute path the sandbox does not
// mount itself, not inside or over a mount (it would cover the mount).
func validateHome(spec Spec) error {
	if spec.Home == "" {
		return nil
	}
	if !path.IsAbs(spec.Home) || path.Clean(spec.Home) != spec.Home || reserved(spec.Home) || spec.Home == "/tmp" {
		return configError("home must be a clean absolute path outside the sandbox's own directories")
	}
	for _, m := range spec.Mounts {
		if t := targetOf(m); t == spec.Home || strings.HasPrefix(spec.Home, t+"/") {
			return configError("home cannot be a mount or lie inside one: " + t)
		}
	}
	return nil
}

func validatePath(dirs []string) error {
	for _, d := range dirs {
		if !path.IsAbs(d) || strings.ContainsAny(d, ":\x00") {
			return configError("path entries must be absolute directories without : or NUL")
		}
	}
	return nil
}

// reservedTargets are mounted by the sandbox itself.
var reservedTargets = []string{"/", "/usr", "/bin", "/lib", "/lib64", "/sbin", "/proc", "/dev", "/tmp", "/etc"}

func validHide(rel string) bool {
	return rel != "." && fs.ValidPath(rel)
}

// Reserved reports whether target is a path inside the sandbox that no
// mount may use (/, /usr, /proc, /tmp and the like).
func Reserved(target string) bool { return reserved(target) }

// MaxHidden bounds the hidden paths of one sandbox, all mounts together:
// each costs a few arguments to the Linux sandbox, which accepts 9000.
const MaxHidden = 1500

func validateHide(hide []string) error {
	for _, h := range hide {
		if !validHide(h) {
			return configError("hidden paths are relative paths inside the mount")
		}
	}
	return nil
}

func validateMounts(mounts []Mount) error {
	seen := map[string]bool{}
	hidden := 0
	for _, m := range mounts {
		target, err := validateMount(m)
		if err != nil {
			return err
		}
		if seen[target] {
			return configError("mount targets must be distinct, clean, absolute paths with a source")
		}
		if hidden += len(m.Hide) + len(m.ReadOnly); hidden > MaxHidden {
			return configError("too many hidden paths across the mounts")
		}
		seen[target] = true
	}
	return nil
}

// validateMount checks one mount and returns its target.
func validateMount(m Mount) (string, error) {
	m.Target = targetOf(m)
	target := path.Clean(m.Target)
	if !path.IsAbs(m.Target) || target != m.Target || m.Source == "" {
		return "", configError("mount targets must be distinct, clean, absolute paths with a source")
	}
	if reserved(target) {
		return "", configError("mount target " + target + " is reserved")
	}
	if err := validateHide(m.Hide); err != nil {
		return "", err
	}
	if err := validateHide(m.ReadOnly); err != nil || len(m.ReadOnly) > 0 && !m.Writable {
		return "", configError("read-only paths are relative paths inside a writable mount")
	}
	return target, nil
}

// targetOf is where m appears inside the sandbox.
func targetOf(m Mount) string {
	if m.Target == "" {
		return filepath.ToSlash(m.Source)
	}
	return m.Target
}

// reserved reports targets the sandbox mounts itself; inside /tmp is free.
func reserved(target string) bool {
	for _, r := range reservedTargets {
		if target == r || r != "/" && r != "/tmp" && strings.HasPrefix(target, r+"/") {
			return true
		}
	}
	return false
}

var reservedEnv = map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true}

// proxyEnv points every common client at the proxy; no_proxy is cleared
// so that nothing bypasses it (there is nothing to bypass to).
var proxyEnv = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy"}

func validateEnv(env map[string]string) error {
	for k, v := range env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) || reservedEnv[k] || isProxyEnv(k) {
			return configError("environment names must be non-empty, without = or NUL, and not PATH, HOME, TMPDIR, LANG or a proxy variable")
		}
	}
	return nil
}

func isProxyEnv(k string) bool {
	for _, p := range proxyEnv {
		if k == p {
			return true
		}
	}
	return false
}

// fixedEnv is the environment the sandbox sets itself.
func fixedEnv(spec Spec, tmp, proxy string) map[string]string {
	env := map[string]string{"PATH": "/usr/bin:/bin", "HOME": tmp, "TMPDIR": tmp, "LANG": "C.UTF-8"}
	if len(spec.Path) > 0 {
		env["PATH"] = strings.Join(spec.Path, ":")
	}
	switch {
	case spec.Home != "":
		env["HOME"] = spec.Home
	case spec.HomeShown != "":
		// The home, granted as it is: ~ finds its files; what tools cache
		// goes to the temporary directory, as the home may be read-only.
		env["HOME"], env["XDG_CACHE_HOME"] = spec.HomeShown, tmp+"/.cache"
	}
	if spec.Network == ProxiedNetwork {
		for _, k := range proxyEnv {
			env[k] = "http://" + proxy
		}
		env["NO_PROXY"], env["no_proxy"] = "", ""
	}
	return env
}

func (l Limits) withDefaults() Limits {
	if l.Memory <= 0 {
		l.Memory = 8 << 30
	}
	if l.Processes <= 0 {
		l.Processes = 256
	}
	if l.CPU <= 0 {
		l.CPU = 60 * time.Second
	}
	if l.FileSize <= 0 {
		l.FileSize = 256 << 20
	}
	if l.OpenFiles <= 0 {
		l.OpenFiles = 256
	}
	return l
}
