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
	t.home, t.homeShown = t.homeDir()
	t.path = t.cfg.Path
	if t.path == nil {
		t.path = t.visiblePath(os.Getenv("PATH"))
	}
	return nil
}

// homeDir is where HOME goes in the sandbox: a private home (home), or,
// when the working directory or a Read path holds the home, the home as it
// is (shown); neither for none.
func (t *tool) homeDir() (home, shown string) {
	home = t.cfg.Home
	if home == "" {
		home, _ = os.UserHomeDir() // #nosec G104 -- no home: HOME stays the temporary directory
	}
	home = filepath.Clean(home)
	if !filepath.IsAbs(home) || sandbox.Reserved(home) || home == "/tmp" {
		return "", ""
	}
	if within(home, t.cfg.Dir) {
		return "", filepath.ToSlash(home)
	}
	for _, r := range t.cfg.Read {
		if within(home, r) {
			return "", filepath.ToSlash(home)
		}
	}
	return filepath.ToSlash(home), ""
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

// runSandboxed runs command in the sandbox. One at a time: hidden and
// read-only paths are found by walking the working directory and then
// mounted by path, so nothing may change it in between; and the
// repository checks after a run must see that run's changes alone.
func (t *tool) runSandboxed(ctx context.Context, command string, out *capture) error {
	p, repos, stop, err := t.startSandboxed(ctx, command, out)
	if err != nil {
		return err
	}
	defer stop()
	t.track(out, repos)
	err = p.Wait()
	t.untrack(out)
	quiet := context.WithoutCancel(ctx)
	out.notes = guardRepos(quiet, t.cfg.Dir, repos, t.workFind)
	out.notes = append(out.notes, t.guardLive(quiet)...)
	return err
}

// track and untrack keep the sandboxed commands running, for guardLive.
func (t *tool) track(out *capture, repos repoState) {
	t.liveMu.Lock()
	defer t.liveMu.Unlock()
	if t.live == nil {
		t.live = map[*capture]liveRun{}
	}
	t.live[out] = liveRun{at: time.Now(), repos: repos}
}

func (t *tool) untrack(out *capture) {
	t.liveMu.Lock()
	defer t.liveMu.Unlock()
	delete(t.live, out)
}

// guardLive undoes what the sandboxed commands still running (in the
// background) did to the repositories so far, against the oldest one's
// start: before git can run on the host (a command outside the sandbox),
// and after each command, not only when they end (audit 21/11: a job's
// planted hook waited for the job's end, which could never come).
func (t *tool) guardLive(ctx context.Context) []string {
	t.liveMu.Lock()
	var oldest *liveRun
	for _, l := range t.live {
		if oldest == nil || l.at.Before(oldest.at) {
			oldest = &l
		}
	}
	t.liveMu.Unlock()
	if oldest == nil {
		return nil
	}
	return guardRepos(ctx, t.cfg.Dir, oldest.repos, t.workFind)
}

// startSandboxed starts command; one starts at a time (the mounts are
// built from the directory as it is then), and runs alongside the others
// (a background server, a build).
func (t *tool) startSandboxed(ctx context.Context, command string, out *capture) (sandbox.Process, repoState, func(), error) {
	t.runMu.Lock()
	defer t.runMu.Unlock()
	spec, repos, err := t.spec(ctx, command, out)
	if err != nil {
		return nil, nil, nil, err
	}
	stop := func() {}
	if t.proxy != nil {
		sock, listening, err := t.proxy.Listen(ctx, out.refused)
		if err != nil {
			return nil, nil, nil, err
		}
		stop = listening
		spec.Network, spec.Proxy = sandbox.ProxiedNetwork, sock
	}
	p, err := t.box.Start(ctx, spec)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	return p, repos, stop, nil
}

func (t *tool) spec(ctx context.Context, command string, out *capture) (sandbox.Spec, repoState, error) {
	mounts, repos, err := t.mounts(ctx)
	if err != nil {
		return sandbox.Spec{}, nil, err
	}
	env := map[string]string{"TERM": "dumb", "PAGER": "cat", "GIT_PAGER": "cat", "GIT_TERMINAL_PROMPT": "0"}
	for k, v := range t.cfg.Env {
		env[k] = v
	}
	return sandbox.Spec{
		Argv:      []string{"/bin/bash", "-c", command},
		Dir:       filepath.ToSlash(t.cfg.Dir),
		Env:       env,
		Path:      t.path,
		Home:      t.home,
		HomeShown: t.homeShown,
		Mounts:    mounts,
		Limits:    t.limits(ctx),
		Protected: t.cfg.Never,
		Stdout:    out,
		Stderr:    out,
	}, repos, nil
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
func (t *tool) mounts(ctx context.Context) ([]sandbox.Mount, repoState, error) {
	work, repos, err := t.workMount(ctx)
	if err != nil {
		return nil, nil, err
	}
	mounts := []sandbox.Mount{work}
	for _, r := range t.cfg.Read {
		m, ok, err := t.readMount(ctx, r)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			mounts = append(mounts, m)
		}
	}
	sort.SliceStable(mounts, func(i, j int) bool { return depth(target(mounts[i])) < depth(target(mounts[j])) })
	hideLinkTargets(mounts)
	fit(mounts)
	return mounts, repos, nil
}

func target(m sandbox.Mount) string {
	if m.Target != "" {
		return m.Target
	}
	return m.Source
}

// readMount grants a Read path at its own path. A symbolic link (a
// dotfile manager's ~/.gitconfig) is resolved here: what it points to is
// granted, where the link is.
func (t *tool) readMount(ctx context.Context, r string) (sandbox.Mount, bool, error) {
	real, err := filepath.EvalSymlinks(r)
	if err != nil || r == t.cfg.Dir || files.Excluded(r, t.cfg.Exclude) || files.Excluded(real, t.cfg.Exclude) {
		return sandbox.Mount{}, false, nil // gone, the working directory itself, or a secret
	}
	info, err := os.Stat(real)
	if err != nil {
		return sandbox.Mount{}, false, nil
	}
	m := sandbox.Mount{Source: real}
	if real != r {
		m.Target = filepath.ToSlash(r)
	}
	if info.IsDir() {
		if m.Hide, err = t.readHidden(real); err != nil {
			return sandbox.Mount{}, false, err
		}
	}
	return m, true, nil
}

func depth(p string) int { return strings.Count(filepath.ToSlash(filepath.Clean(p)), "/") }

// workMount is the working directory, writable, with its secrets hidden
// (walked each time: they come and go) and its protected paths read-only.
// It also returns the repositories found, for the checks after the run.
func (t *tool) workMount(ctx context.Context) (sandbox.Mount, repoState, error) {
	hide, err := t.workHidden(ctx)
	if err != nil {
		return sandbox.Mount{}, nil, err
	}
	hide = append(hide, t.neverIn(t.cfg.Dir)...) // run from the home: its configuration in it
	protected := t.cfg.Protected
	if protected == nil {
		protected = files.DefaultProtected
	}
	found, err := t.workFind(ctx, protected)
	if err != nil {
		return sandbox.Mount{}, nil, err
	}
	var ro []string
	for _, p := range found {
		ro = append(ro, readOnly(t.cfg.Dir, p)...)
	}
	repos, err := scanRepos(ctx, t.cfg.Dir, t.workFind)
	if err != nil {
		return sandbox.Mount{}, nil, err
	}
	return sandbox.Mount{Source: t.cfg.Dir, Writable: true, Hide: hide, ReadOnly: ro}, repos, nil
}

// readOnly maps a protected path to what stays read-only: of a repository
// only what makes git run code or look elsewhere for it, so that
// committing works.
func readOnly(dir, rel string) []string {
	if path.Base(rel) != ".git" {
		return []string{rel}
	}
	info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil || !info.IsDir() {
		return []string{rel} // a worktree's .git file points elsewhere: kept as it is
	}
	out := make([]string, 0, len(gitGuarded))
	for _, g := range gitGuarded {
		out = append(out, rel+"/"+g)
	}
	return out
}

// workHidden is what the working directory hides: a project's secrets
// wherever they are (walked at each command: they come and go), or, when
// it is the home itself, its secrets where they lie (a home is not walked,
// as a Read directory is not: two million entries took seconds).
func (t *tool) workHidden(ctx context.Context) ([]string, error) {
	return t.workFind(ctx, t.cfg.Exclude)
}

// workFind is what patterns (nil: the exclusion defaults) match in the
// working directory: all of a project, walked; of the home, where they lie
// (the protected startup files and .git as its secrets: the home is not
// walked at each command).
func (t *tool) workFind(ctx context.Context, patterns []string) ([]string, error) {
	if t.homeShown != "" && filepath.Clean(filepath.FromSlash(t.homeShown)) == t.cfg.Dir {
		return files.Anchored(t.cfg.Dir, patterns)
	}
	return files.Hidden(ctx, t.cfg.Dir, patterns)
}

// readHidden is what a Read directory hides: the secrets where they lie
// at its top (files.Anchored, no walk: a home of two million entries took
// seconds and was refused past one million), and the protected paths
// inside it (the agent's own configuration in the home), which the
// sandbox requires hidden.
func (t *tool) readHidden(dir string) ([]string, error) {
	hide, err := files.Anchored(dir, t.cfg.Exclude)
	if err != nil {
		return nil, err
	}
	return append(hide, t.neverIn(dir)...), nil
}

// neverIn is the protected paths inside dir, relative: hidden there, the
// sandbox grants dir.
func (t *tool) neverIn(dir string) []string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real // both sides resolved (macOS: /var is /private/var)
	}
	var out []string
	for _, p := range t.cfg.Never {
		real := p
		if r, err := filepath.EvalSymlinks(p); err == nil {
			real = r
		}
		if rel, err := filepath.Rel(dir, real); err == nil && rel != "." && filepath.IsLocal(rel) {
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return out
}

// runHost runs command outside the sandbox, with the user's rights, in the
// working directory. Variables that look like credentials are not passed:
// the user allowed a command, not the agent's keys.
func (t *tool) runHost(ctx context.Context, command string, out *capture) error {
	out.notes = t.guardLive(context.WithoutCancel(ctx)) // git here must not find what a sandboxed job planted
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
