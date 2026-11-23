// darwin.go: the Seatbelt backend
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build darwin

package sandbox

import (
	"context"
	stderrors "errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/agilira/go-errors"
)

// sandboxExecPath runs a program under a Seatbelt profile: the kernel's
// own sandbox, which Safari, Chrome and Claude Code use on macOS too.
const sandboxExecPath = "/usr/bin/sandbox-exec"

// The macOS backend differs from Linux's where Seatbelt cannot do what
// namespaces do:
//   - nothing can be placed elsewhere: a mount's Target must be its
//     Source, or a symbolic link to it;
//   - rules follow paths, not mounts: every path the program could
//     rename to escape a rule (above a hidden or read-only path) is pinned;
//   - HOME is Spec.Home but stays read-only (no private directory can be
//     put there), and /tmp is a private temporary directory, in TMPDIR;
//   - host processes are visible, and the limits are CPU time, file size
//     and open files (no address-space or process cap).
type darwinSandbox struct{}

func newPlatform() (Sandbox, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New(ErrUnavailable, "the sandbox refuses to run as root")
	}
	if !executable(sandboxExecPath) {
		return nil, errors.New(ErrUnavailable, sandboxExecPath+" is required")
	}
	return darwinSandbox{}, nil
}

func (darwinSandbox) Start(ctx context.Context, spec Spec) (Process, error) {
	if err := validate(spec); err != nil {
		return nil, err
	}
	run := &darwinRun{}
	p, err := run.start(ctx, spec)
	if err != nil {
		run.release()
		return nil, err
	}
	return p, nil
}

// darwinRun is what one start holds: the private temporary directory and
// the proxy's loopback bridge.
type darwinRun struct {
	tmp    string
	bridge *loopBridge
}

func (r *darwinRun) release() error {
	var errs []error
	if r.bridge != nil {
		errs = append(errs, r.bridge.close())
	}
	if r.tmp != "" {
		errs = append(errs, os.RemoveAll(r.tmp))
	}
	return stderrors.Join(errs...)
}

func (r *darwinRun) start(ctx context.Context, spec Spec) (Process, error) {
	tmp, err := os.MkdirTemp("", "ringil-sandbox-")
	if err != nil {
		return nil, errors.Wrap(err, ErrStart, "cannot create the sandbox's temporary directory")
	}
	r.tmp = tmp
	if r.tmp, err = filepath.EvalSymlinks(tmp); err != nil { // /var is /private/var
		r.tmp = tmp
		return nil, errors.Wrap(err, ErrStart, "cannot resolve the temporary directory")
	}
	grants, err := resolveGrants(spec)
	if err != nil {
		return nil, err
	}
	proxy := ""
	if spec.Network == ProxiedNetwork {
		if r.bridge, err = newLoopBridge(spec.Proxy); err != nil {
			return nil, err
		}
		proxy = r.bridge.addr()
	}
	profile := buildProfile(spec, grants, r.tmp, proxy)
	cmd := exec.CommandContext(ctx, sandboxExecPath, append([]string{"-p", profile, "/bin/sh", "-c", limitScript(spec.Limits.withDefaults()), "ringil-sandbox"}, spec.Argv...)...) // #nosec G204 -- fixed binary; argv runs inside the profile
	cmd.Dir = spec.Dir
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	cmd.Env = darwinEnv(spec, r.tmp, proxy)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return nil, errors.Wrap(err, ErrStart, "cannot start sandbox-exec")
	}
	return &darwinProcess{cmd: cmd, run: r}, nil
}

type darwinProcess struct {
	cmd *exec.Cmd
	run *darwinRun
}

// Wait returns the program's exit error first, joined with any failure to
// release the sandbox. The process group goes with the program.
func (p *darwinProcess) Wait() error {
	err := p.cmd.Wait()
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL) // #nosec G104 -- stragglers of the group; usually gone
	return stderrors.Join(err, p.run.release())
}

// grant is a mount as Seatbelt sees it: the real path.
type grant struct {
	Mount
	real string
	dir  bool
}

// resolveGrants resolves every mount, refuses those that overlap protected
// data, and those placed elsewhere than their own path.
func resolveGrants(spec Spec) ([]grant, error) {
	var protected []string
	for _, p := range spec.Protected {
		real, err := resolve(p)
		if err != nil {
			return nil, err
		}
		protected = append(protected, real)
	}
	var out []grant
	for _, m := range spec.Mounts {
		g, err := resolveGrant(m, protected)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func resolveGrant(m Mount, protected []string) (grant, error) {
	real, err := filepath.EvalSymlinks(m.Source)
	if err != nil {
		return grant{}, errors.New(ErrGrant, "cannot open granted path "+m.Source)
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() && !info.Mode().IsRegular() {
		return grant{}, errors.New(ErrGrant, "granted paths are directories or regular files: "+m.Source)
	}
	if err := checkGrant(m, real, info.IsDir(), protected); err != nil {
		return grant{}, err
	}
	return grant{Mount: m, real: real, dir: info.IsDir()}, nil
}

// checkGrant: a grant appears at its own path, holds hidden and read-only
// paths only as a directory, and stays clear of protected data.
func checkGrant(m Mount, real string, dir bool, protected []string) error {
	if !atOwnPath(m, real) {
		return configError("on macOS a granted path appears only at its own path: " + m.Target)
	}
	if !dir && len(m.Hide)+len(m.ReadOnly) > 0 {
		return configError("hidden and read-only paths need a directory mount: " + m.Source)
	}
	for _, p := range protected {
		if within(real, p) || within(p, real) && !masked(m, real, p) {
			return errors.New(ErrGrant, "a granted directory overlaps protected data: "+m.Source)
		}
	}
	return nil
}

// atOwnPath: no Target, the Source itself, or a link to it.
func atOwnPath(m Mount, real string) bool {
	if m.Target == "" || m.Target == m.Source {
		return true
	}
	t, err := filepath.EvalSymlinks(m.Target)
	return err == nil && t == real
}

func darwinEnv(spec Spec, tmp, proxy string) []string {
	env := fixedEnv(spec, tmp, proxy)
	for k, v := range spec.Env {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// limitScript applies the limits Seatbelt's platform has, then runs the
// program: ulimit's file size counts 512-byte blocks.
func limitScript(l Limits) string {
	return "ulimit -c 0; ulimit -t " + strconv.Itoa(int((l.CPU+time.Second-1)/time.Second)) +
		"; ulimit -f " + strconv.FormatInt(max(l.FileSize/512, 1), 10) +
		"; ulimit -n " + strconv.Itoa(l.OpenFiles) + ` 2>/dev/null; exec "$@"`
}
