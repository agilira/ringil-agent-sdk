// run.go: running a command, in the sandbox or on the host
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"context"
	stderrors "errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/sandbox"
	"github.com/agilira/ringil-agent-sdk/tools/files"
)

// settle fixes the home and the PATH the sandbox uses.
func (t *tool) settle() error {
	for i, r := range t.cfg.Read {
		abs, err := filepath.Abs(r)
		if err != nil {
			return errors.Wrap(err, ErrConfig, "cannot resolve "+r)
		}
		t.cfg.Read[i] = abs
	}
	t.home = t.homeDir()
	t.path = t.cfg.Path
	if t.path == nil {
		t.path = t.visiblePath(os.Getenv("PATH"))
	}
	return nil
}

// homeDir is where HOME goes in the sandbox, "" for none: not when the
// working directory or a Read path holds the home (it is shown as it is).
func (t *tool) homeDir() string {
	home := t.cfg.Home
	if home == "" {
		home, _ = os.UserHomeDir() // #nosec G104 -- no home: HOME stays the temporary directory
	}
	home = filepath.Clean(home)
	if !filepath.IsAbs(home) || within(home, t.cfg.Dir) || sandbox.Reserved(home) || home == "/tmp" {
		return ""
	}
	for _, r := range t.cfg.Read {
		if within(home, r) {
			return ""
		}
	}
	return filepath.ToSlash(home)
}

// visiblePath keeps the host's PATH entries a sandboxed command can see:
// the system's and those inside the working directory or a Read path.
func (t *tool) visiblePath(hostPath string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range filepath.SplitList(hostPath) {
		if filepath.IsAbs(d) && !seen[d] && !strings.ContainsAny(d, ":\x00") && t.visible(d) {
			seen[d] = true
			out = append(out, filepath.ToSlash(d))
		}
	}
	return out
}

func (t *tool) visible(d string) bool {
	if within(d, "/usr") || d == "/bin" || d == "/sbin" || within(d, t.cfg.Dir) {
		return true
	}
	for _, r := range t.cfg.Read {
		if within(d, r) {
			return true
		}
	}
	return false
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// runSandboxed runs command in the sandbox.
func (t *tool) runSandboxed(ctx context.Context, command string, out *capture) error {
	spec, err := t.spec(ctx, command, out)
	if err != nil {
		return err
	}
	if t.proxy != nil {
		sock, stop, err := t.proxy.Listen(ctx, out.refused)
		if err != nil {
			return err
		}
		defer stop()
		spec.Network, spec.Proxy = sandbox.ProxiedNetwork, sock
	}
	p, err := t.box.Start(ctx, spec)
	if err != nil {
		return err
	}
	return p.Wait()
}

func (t *tool) spec(ctx context.Context, command string, out *capture) (sandbox.Spec, error) {
	mounts, err := t.mounts(ctx)
	if err != nil {
		return sandbox.Spec{}, err
	}
	env := map[string]string{"TERM": "dumb", "PAGER": "cat", "GIT_PAGER": "cat", "GIT_TERMINAL_PROMPT": "0"}
	for k, v := range t.cfg.Env {
		env[k] = v
	}
	return sandbox.Spec{
		Argv:      []string{"/usr/bin/bash", "-c", command},
		Dir:       filepath.ToSlash(t.cfg.Dir),
		Env:       env,
		Path:      t.path,
		Home:      t.home,
		Mounts:    mounts,
		Limits:    t.limits(ctx),
		Protected: t.cfg.Never,
		Stdout:    out,
		Stderr:    out,
	}, nil
}

// limits raise the sandbox's defaults for builds: more processes, and CPU
// time for every core over the whole timeout.
func (t *tool) limits(ctx context.Context) sandbox.Limits {
	l := t.cfg.Limits
	if l.Processes == 0 {
		l.Processes = 1024
	}
	if l.OpenFiles == 0 {
		l.OpenFiles = 1024
	}
	if l.CPU == 0 {
		left := MaxTimeout
		if deadline, ok := ctx.Deadline(); ok {
			left = time.Until(deadline)
		}
		l.CPU = max(left, time.Second) * time.Duration(runtime.NumCPU())
	}
	return l
}

// mounts grants the Read paths, then the working directory: deepest last,
// so that a mount inside another shows over it.
func (t *tool) mounts(ctx context.Context) ([]sandbox.Mount, error) {
	work, err := t.workMount(ctx)
	if err != nil {
		return nil, err
	}
	mounts := []sandbox.Mount{work}
	for _, r := range t.cfg.Read {
		if r == t.cfg.Dir || files.Excluded(r, t.cfg.Exclude) {
			continue
		}
		info, err := os.Stat(r)
		if err != nil {
			continue // gone: nothing to grant
		}
		m := sandbox.Mount{Source: r}
		if info.IsDir() {
			if m.Hide, err = t.hiddenOf(ctx, r); err != nil {
				return nil, err
			}
		}
		mounts = append(mounts, m)
	}
	sort.SliceStable(mounts, func(i, j int) bool { return depth(mounts[i].Source) < depth(mounts[j].Source) })
	return mounts, nil
}

func depth(p string) int { return strings.Count(filepath.ToSlash(filepath.Clean(p)), "/") }

// workMount is the working directory, writable, with its secrets hidden
// (walked each time: they come and go) and its protected paths read-only.
func (t *tool) workMount(ctx context.Context) (sandbox.Mount, error) {
	hide, err := files.Hidden(ctx, t.cfg.Dir, t.cfg.Exclude)
	if err != nil {
		return sandbox.Mount{}, err
	}
	protected := t.cfg.Protected
	if protected == nil {
		protected = files.DefaultProtected
	}
	found, err := files.Hidden(ctx, t.cfg.Dir, protected)
	if err != nil {
		return sandbox.Mount{}, err
	}
	var ro []string
	for _, p := range found {
		ro = append(ro, readOnly(t.cfg.Dir, p)...)
	}
	return sandbox.Mount{Source: t.cfg.Dir, Writable: true, Hide: hide, ReadOnly: ro}, nil
}

// readOnly maps a protected path to what stays read-only: of a repository
// only what makes git run code (hooks, configuration, info/) is, so that
// committing works.
func readOnly(dir, rel string) []string {
	if path.Base(rel) != ".git" {
		return []string{rel}
	}
	info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil || !info.IsDir() {
		return []string{rel} // a worktree's .git file points elsewhere: kept as it is
	}
	return []string{rel + "/hooks", rel + "/config", rel + "/info"}
}

// hiddenOf walks a Read directory once for the tool's life: toolchains
// are large and change little.
func (t *tool) hiddenOf(ctx context.Context, dir string) ([]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if h, ok := t.hidden[dir]; ok {
		return h, nil
	}
	h, err := files.Hidden(ctx, dir, t.cfg.Exclude)
	if err != nil {
		return nil, err
	}
	t.hidden[dir] = h
	return h, nil
}

// runHost runs command outside the sandbox, with the user's rights, in the
// working directory. Variables that look like credentials are not passed:
// the user allowed a command, not the agent's keys.
func (t *tool) runHost(ctx context.Context, command string, out *capture) error {
	shell, err := exec.LookPath("bash")
	if err != nil {
		return agent.ToolError(ErrRun, "There is no bash on this machine.")
	}
	cmd := exec.CommandContext(ctx, shell, "-c", command) // #nosec G204 -- the user approved this command
	cmd.Dir = t.cfg.Dir
	cmd.Env = hostEnv(os.Environ())
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 2 * time.Second
	group(cmd)
	return cmd.Run()
}

// credentialWords mark variable names that hold secrets.
var credentialWords = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "AUTH", "COOKIE", "SESSION"}

func hostEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		secret := false
		for _, w := range credentialWords {
			if strings.Contains(upper, w) && upper != "SSH_AUTH_SOCK" {
				secret = true
			}
		}
		if !secret {
			out = append(out, kv)
		}
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "PAGER=cat", "GIT_PAGER=cat")
}

// exitStatus turns a finished command's error into its exit status; an
// error that is not an exit (the sandbox could not start) is returned.
func exitStatus(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if stderrors.As(err, &exit) {
		if code := exit.ExitCode(); code >= 0 {
			return code, nil
		}
		return 128 + 9, nil // killed: the timeout, or a limit
	}
	var coded *errors.Error
	if stderrors.As(err, &coded) {
		return 0, agent.ToolError(ErrRun, "The command could not start: "+coded.Message)
	}
	return 0, agent.ToolError(ErrRun, "The command could not start: "+err.Error())
}

// ReadDefaults are the host paths a developer's commands usually need to
// read, those that exist: git's settings and the toolchains the PATH
// finds under home (the directory above a bin, ~/sdk/go/bin giving
// ~/sdk/go, so the standard library and module cache come along).
func ReadDefaults() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	out := addExisting(nil, filepath.Join(home, ".gitconfig"))
	out = addExisting(out, filepath.Join(home, ".config", "git"))
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(d) && within(d, home) && d != home {
			out = addExisting(out, toolchain(d, home))
		}
	}
	return out
}

// toolchain is the directory above a PATH entry's bin, unless that is the
// home or ~/.local (too wide to grant whole).
func toolchain(d, home string) string {
	parent := filepath.Dir(d)
	if filepath.Base(d) != "bin" || parent == home || parent == filepath.Join(home, ".local") {
		return d
	}
	return parent
}

// addExisting adds p when it exists and no listed path holds it.
func addExisting(out []string, p string) []string {
	for _, o := range out {
		if within(p, o) {
			return out
		}
	}
	if _, err := os.Stat(p); err != nil { // #nosec G703 -- a path the host chose; only its existence is read
		return out
	}
	return append(out, p)
}
