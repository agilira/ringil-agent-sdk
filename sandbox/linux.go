// linux.go: the bubblewrap backend
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package sandbox

import (
	"context"
	stderrors "errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"
	"golang.org/x/sys/unix"
)

// Fixed system binaries: the sandbox never runs whatever is first on PATH.
const (
	bwrapPath   = "/usr/bin/bwrap"
	prlimitPath = "/usr/bin/prlimit"
	slirpPath   = "/usr/bin/slirp4netns"
	nsenterPath = "/usr/bin/nsenter"
	socatPath   = "/usr/bin/socat"
)

type linuxSandbox struct{}

func newPlatform() (Sandbox, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New(ErrUnavailable, "the sandbox refuses to run as root")
	}
	for _, bin := range []string{bwrapPath, prlimitPath} {
		if !executable(bin) {
			return nil, errors.New(ErrUnavailable, bin+" is required (install bubblewrap and util-linux)")
		}
	}
	if _, err := auditArch(); err != nil {
		return nil, err
	}
	return linuxSandbox{}, nil
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// launch is everything opened for one start, released together.
type launch struct {
	files []*os.File // passed to bwrap, in fd order from 3
	extra []*os.File // kept by the host side (network pipes)
}

// close releases every descriptor and reports the first failure.
func (l *launch) close() error {
	var first error
	for _, f := range append(l.files, l.extra...) {
		if err := f.Close(); err != nil && first == nil && !stderrors.Is(err, os.ErrClosed) {
			first = err
		}
	}
	l.files, l.extra = nil, nil
	return first
}

// failed releases a launch that will not run and returns err, keeping its
// code and recording a release failure alongside.
func (l *launch) failed(err error) error {
	cerr := l.close()
	var coded *errors.Error
	if cerr != nil && stderrors.As(err, &coded) {
		return coded.WithContext("release_error", cerr.Error())
	}
	return err
}

func (linuxSandbox) Start(ctx context.Context, spec Spec) (Process, error) {
	if err := validate(spec); err != nil {
		return nil, err
	}
	if spec.Network == IsolatedNetwork && !executable(slirpPath) {
		return nil, errors.New(ErrUnavailable, "isolated network needs "+slirpPath)
	}
	if spec.Network == ProxiedNetwork && (!executable(nsenterPath) || !executable(socatPath)) {
		return nil, errors.New(ErrUnavailable, "a proxied network needs "+nsenterPath+" and "+socatPath+" (install util-linux and socat)")
	}
	l := &launch{}
	args, err := l.prepare(spec)
	if err != nil {
		return nil, l.failed(err)
	}
	return l.start(ctx, spec, args)
}

// prepare opens the grants, the seccomp filter and the network pipes, and
// builds bwrap's arguments. Descriptor numbers follow the order files are
// added: 3 for the first.
func (l *launch) prepare(spec Spec) ([]string, error) {
	args := baseArgs()
	mounts, err := l.mountArgs(spec)
	if err != nil {
		return nil, err
	}
	args = append(args, mounts...)
	netArgs, err := l.networkArgs(spec.Network)
	if err != nil {
		return nil, err
	}
	args = append(args, netArgs...)
	filter, err := seccompFilter(spec.Network != NoNetwork)
	if err != nil {
		return nil, err
	}
	args = append(args, "--seccomp", l.add(filter))
	args = append(args, envArgs(spec)...)
	dir := spec.Dir
	if dir == "" {
		dir = "/"
	}
	args = append(args, "--remount-ro", "/dev", "--remount-ro", "/", "--chdir", dir, "--")
	return append(args, limitArgs(spec.Limits.withDefaults(), spec.Argv)...), nil
}

// add passes f to bwrap and returns its descriptor number there.
func (l *launch) add(f *os.File) string {
	l.files = append(l.files, f)
	return strconv.Itoa(2 + len(l.files))
}

func baseArgs() []string {
	args := []string{
		"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL",
		// A fresh user namespace mapped to the caller, in which no further
		// namespaces can be made: the kernel's userns surface stays closed.
		"--unshare-user", "--disable-userns", "--uid", strconv.Itoa(os.Getuid()), "--gid", strconv.Itoa(os.Getgid()),
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64",
		"--proc", "/proc", "--dev", "/dev",
		// /dev is a tmpfs without a size of its own: shared memory gets a
		// sized one, and /dev is made read-only below.
		"--size", strconv.Itoa(64 << 20), "--tmpfs", "/dev/shm",
		"--size", strconv.Itoa(256 << 20), "--tmpfs", "/tmp",
		"--dir", "/etc",
	}
	// The system's runtime configuration, nothing of the user's: library
	// alternatives (Debian's libblas.so.3 is a link through
	// /etc/alternatives), the loader cache, time zone, fonts and CA
	// certificates. Read-only, and skipped where absent.
	for _, p := range systemConfig {
		args = append(args, "--ro-bind-try", p, p)
	}
	return args
}

var systemConfig = []string{
	"/etc/alternatives", "/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	"/etc/localtime", "/etc/fonts", "/etc/ssl", "/etc/ca-certificates", "/etc/pki",
	// who the user is (git and ssh look the name up) and the system's
	// defaults for git; no secrets live in these
	"/etc/passwd", "/etc/group", "/etc/nsswitch.conf", "/etc/gitconfig", "/etc/mime.types",
}

// mountArgs opens every granted directory and binds the open descriptor,
// not the path: a path can be swapped for a symlink between the check and
// the mount, a descriptor cannot.
func (l *launch) mountArgs(spec Spec) ([]string, error) {
	var args []string
	if spec.Home != "" {
		// First, so that mounts inside it land on it.
		args = append(args, "--size", strconv.Itoa(256<<20), "--perms", "0700", "--tmpfs", spec.Home)
	}
	for _, m := range spec.Mounts {
		f, dir, err := openSource(m.Source)
		if err != nil {
			return nil, err
		}
		fd := l.add(f)
		if err := checkProtected(f, spec.Protected); err != nil {
			return nil, err
		}
		if !dir && len(m.Hide)+len(m.ReadOnly) > 0 {
			return nil, configError("hidden and read-only paths need a directory mount: " + m.Source)
		}
		bind := "--ro-bind-fd"
		if m.Writable {
			bind = "--bind-fd"
		}
		args = append(args, bind, fd, targetOf(m))
		masks, err := l.innerArgs(m)
		if err != nil {
			return nil, err
		}
		args = append(args, masks...)
	}
	return args, nil
}

// innerArgs keeps a mount's read-only paths read-only, then masks its
// hidden paths (a hidden path inside a read-only one stays hidden).
func (l *launch) innerArgs(m Mount) ([]string, error) {
	if len(m.Hide)+len(m.ReadOnly) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(m.Source)
	if err != nil {
		return nil, errors.Wrap(err, ErrGrant, "cannot open "+m.Source)
	}
	defer closeQuiet(root)
	var args []string
	for _, rel := range m.ReadOnly {
		info, err := root.Lstat(rel)
		if err != nil || !info.IsDir() && !info.Mode().IsRegular() {
			continue // absent, or a link: it resolves to a path covered or not
		}
		f, err := root.Open(rel)
		if err != nil {
			return nil, errors.Wrap(err, ErrGrant, "cannot open "+rel+" in "+m.Source)
		}
		args = append(args, "--ro-bind-fd", l.add(f), path.Join(targetOf(m), rel))
	}
	return append(args, hideArgs(root, m)...), nil
}

// hideArgs covers each hidden path with an empty read-only directory or
// /dev/null, after the mount that contains it.
func hideArgs(root *os.Root, m Mount) []string {
	var args []string
	for _, h := range m.Hide {
		info, err := root.Lstat(h)
		if err != nil {
			continue // gone: nothing to hide
		}
		dest := path.Join(targetOf(m), h)
		switch {
		case info.IsDir():
			args = append(args, "--perms", "0500", "--tmpfs", dest, "--remount-ro", dest)
		case info.Mode().IsRegular():
			args = append(args, "--ro-bind", "/dev/null", dest)
		}
		// a symbolic link stays: it resolves inside the sandbox, where
		// its target is either another hidden path or not mounted
	}
	return args
}

// openSource opens a granted directory, or a granted regular file, and
// reports which. A file is opened as a path only (never read, so a FIFO
// cannot block) and never through a final symbolic link.
func openSource(source string) (*os.File, bool, error) {
	info, err := os.Lstat(source)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		f, err := openDir(source)
		return f, true, err
	}
	fd, err := unix.Open(source, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, errors.New(ErrGrant, "cannot open granted file "+source)
	}
	f := os.NewFile(uintptr(fd), source)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, false, closeWith(errors.New(ErrGrant, "granted files must be regular files: "+source), f)
	}
	return f, false, nil
}

func openDir(source string) (*os.File, error) {
	root, err := os.OpenRoot(source)
	if err != nil {
		return nil, errors.New(ErrGrant, "cannot open granted directory "+source)
	}
	f, err := root.Open(".")
	if cerr := root.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return nil, errors.New(ErrGrant, "cannot open granted directory "+source)
	}
	return f, nil
}

// checkProtected resolves what the descriptor really points at and refuses
// it when it contains, or lies inside, a protected path. Protected paths
// are resolved too, including through symlinked parents of a file that does
// not exist yet, which the sandbox could otherwise create.
func checkProtected(f *os.File, protected []string) error {
	actual, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
	if err != nil || strings.HasSuffix(actual, " (deleted)") {
		return errors.New(ErrGrant, "cannot resolve a granted directory")
	}
	for _, p := range protected {
		real, err := resolve(p)
		if err != nil {
			return err
		}
		if within(actual, real) || within(real, actual) {
			return errors.New(ErrGrant, "a granted directory overlaps protected data: "+p)
		}
	}
	return nil
}

// resolve returns p with every existing component's symlinks resolved and
// the missing tail appended.
func resolve(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", errors.New(ErrConfig, "protected paths must be absolute: "+p)
	}
	p = filepath.Clean(p)
	var tail []string
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{real}, tail...)...), nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p, nil
		}
		tail = append([]string{filepath.Base(p)}, tail...)
		p = parent
	}
}

func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func envArgs(spec Spec) []string {
	env := fixedEnv(spec, "/tmp")
	for k, v := range spec.Env {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := []string{"--clearenv"}
	for _, k := range keys {
		args = append(args, "--setenv", k, env[k])
	}
	return args
}

// limitArgs wraps argv in prlimit, applied inside the sandbox to the
// program and everything it starts.
func limitArgs(l Limits, argv []string) []string {
	return append([]string{prlimitPath,
		"--as=" + strconv.FormatInt(l.Memory, 10),
		"--nproc=" + strconv.Itoa(l.Processes),
		"--cpu=" + strconv.Itoa(int((l.CPU+time.Second-1)/time.Second)),
		"--fsize=" + strconv.FormatInt(l.FileSize, 10),
		"--nofile=" + strconv.Itoa(l.OpenFiles),
		"--core=0", "--"}, argv...)
}

func (l *launch) start(ctx context.Context, spec Spec, args []string) (Process, error) {
	cmd := exec.CommandContext(ctx, bwrapPath, args...) // #nosec G204 -- fixed binary, argv never reaches a shell
	cmd.ExtraFiles = l.files
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return nil, l.failed(errors.Wrap(err, ErrStart, "cannot start bubblewrap"))
	}
	p := &process{cmd: cmd, launch: l}
	if spec.Network != NoNetwork {
		if err := l.connect(ctx, p, spec); err != nil {
			return nil, p.abort(err)
		}
	}
	return p, nil
}

type process struct {
	cmd    *exec.Cmd
	launch *launch
	net    *exec.Cmd // slirp4netns, or the proxy's bridge
	bridge bool
}

// Wait returns the program's exit error first. A failure to release the
// sandbox afterwards is joined to it, never hidden.
func (p *process) Wait() error {
	err := p.cmd.Wait()
	cerr := p.launch.close()
	var nerr error
	if p.net != nil {
		// Closing our end of the exit pipe (in close) stops slirp4netns;
		// the proxy's bridge is stopped by hand.
		if p.bridge {
			_ = p.net.Process.Kill() // #nosec G104 -- it may be gone already; Wait reports the rest
		}
		nerr = p.net.Wait()
	}
	return stderrors.Join(err, cerr, ignoreExit(nerr))
}

// ignoreExit drops slirp4netns' exit status once the sandbox is gone: it is
// killed on purpose. Other failures (it never ran) are kept.
func ignoreExit(err error) error {
	var exit *exec.ExitError
	if stderrors.As(err, &exit) {
		return nil
	}
	return err
}

// abort kills a sandbox whose start failed half-way and returns cause,
// recording a failure to clean up alongside.
func (p *process) abort(cause error) error {
	kerr := p.cmd.Process.Kill()
	werr := p.Wait()
	var coded *errors.Error
	if cleanup := stderrors.Join(kerr, werr); cleanup != nil && stderrors.As(cause, &coded) {
		return coded.WithContext("cleanup_error", cleanup.Error())
	}
	return cause
}

func closeQuiet(r *os.Root) {
	if err := r.Close(); err != nil {
		return // a read-only handle: nothing to lose
	}
}
