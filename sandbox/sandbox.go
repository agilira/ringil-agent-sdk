// sandbox.go: running untrusted programs in isolation
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package sandbox runs a program isolated from the host: only the
// directories granted to it are visible, read-only unless granted
// writable; it sees no environment it was not given, no host processes,
// and by default no network. Skills' scripts, MCP stdio servers and the
// sandboxed Bash tool all run through it.
//
// The Linux backend uses bubblewrap (namespaces, no capabilities, a
// seccomp filter) with per-process resource limits. Other platforms report
// ErrUnavailable: there is never a silent fallback to running unconfined.
package sandbox

import (
	"context"
	"io"
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
)

// Mount grants a host directory to the sandbox.
type Mount struct {
	// Source is the host directory.
	Source string
	// Target is the absolute path inside the sandbox; empty means the
	// same path as Source, so host paths the user mentions work as they are.
	Target string
	// Writable lets the program change the directory's contents.
	Writable bool
}

// Limits are per-process resource limits. Zero means the default given.
// They are not a total: many processes, or memory held outside the address
// space (memfd), can add up past them. A cgroup-based total is planned;
// until then bound the run with the context's deadline.
type Limits struct {
	// Memory caps the address space of each process; default 1 GiB.
	Memory int64
	// Processes caps the processes of the sandbox user; default 64.
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
	// Env is the whole environment, besides PATH, HOME, TMPDIR and LANG.
	Env     map[string]string
	Mounts  []Mount
	Network Network
	Limits  Limits
	// Protected are absolute host paths that no mount may contain or be
	// inside: credentials, session stores, the agent's own configuration.
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
	return validateEnv(spec.Env)
}

// reservedTargets are mounted by the sandbox itself.
var reservedTargets = []string{"/", "/usr", "/bin", "/lib", "/lib64", "/sbin", "/proc", "/dev", "/tmp", "/etc"}

func validateMounts(mounts []Mount) error {
	seen := map[string]bool{}
	for _, m := range mounts {
		m.Target = targetOf(m)
		target := path.Clean(m.Target)
		if !path.IsAbs(m.Target) || target != m.Target || seen[target] || m.Source == "" {
			return configError("mount targets must be distinct, clean, absolute paths with a source")
		}
		if reserved(target) {
			return configError("mount target " + target + " is reserved")
		}
		seen[target] = true
	}
	return nil
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

func validateEnv(env map[string]string) error {
	for k, v := range env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) || reservedEnv[k] {
			return configError("environment names must be non-empty, without = or NUL, and not PATH, HOME, TMPDIR or LANG")
		}
	}
	return nil
}

func (l Limits) withDefaults() Limits {
	if l.Memory <= 0 {
		l.Memory = 1 << 30
	}
	if l.Processes <= 0 {
		l.Processes = 64
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
