// linux_test.go: the bubblewrap backend, exercised for real
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package sandbox

import (
	"bytes"
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"
)

func sandboxOrSkip(t *testing.T) Sandbox {
	t.Helper()
	s, err := New()
	if errors.HasCode(err, ErrUnavailable) && os.Getenv("RINGIL_REQUIRE_SANDBOX") == "" {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// run starts spec and returns stdout, stderr and the wait error.
func run(t *testing.T, s Sandbox, spec Spec) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return runCtx(ctx, t, s, spec)
}

func runCtx(ctx context.Context, t *testing.T, s Sandbox, spec Spec) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	spec.Stdout, spec.Stderr = &out, &errOut
	p, err := s.Start(ctx, spec)
	if err != nil {
		var coded *errors.Error
		if stderrors.As(err, &coded) {
			t.Fatalf("start: %v (context %v)", err, coded.Context)
		}
		t.Fatalf("start: %v", err)
	}
	werr := p.Wait()
	return out.String(), errOut.String(), werr
}

func sh(script string) []string { return []string{"/usr/bin/bash", "-c", script} }

func TestRunsAndIsolatesEnvironment(t *testing.T) {
	s := sandboxOrSkip(t)
	t.Setenv("RINGIL_HOST_SECRET", "leak")
	out, errOut, err := run(t, s, Spec{
		Argv: sh(`echo "home=$HOME secret=${RINGIL_HOST_SECRET:-none} given=$GIVEN"; ls /home 2>&1 | head -1; ps -e | wc -l`),
		Env:  map[string]string{"GIVEN": "yes"},
	})
	if err != nil {
		t.Fatalf("%v: %s", err, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "home=/tmp secret=none given=yes" {
		t.Fatalf("environment: %q", lines[0])
	}
	if !strings.Contains(lines[1], "No such file") {
		t.Fatalf("host /home visible: %q", lines[1])
	}
	if n, err := strconv.Atoi(strings.TrimSpace(lines[2])); err != nil || n > 6 {
		t.Fatalf("host processes visible: %q %v", lines[2], err)
	}
}

func TestMounts(t *testing.T) {
	s := sandboxOrSkip(t)
	ro, rw := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ro, "in.txt"), []byte("dati"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := run(t, s, Spec{
		Argv:   sh(`cat /in/in.txt; echo x > /in/new 2>/dev/null && echo ro-writable; echo scritto > /out/res.txt`),
		Mounts: []Mount{{Source: ro, Target: "/in"}, {Source: rw, Target: "/out", Writable: true}},
		Dir:    "/in",
	})
	if err != nil {
		t.Fatalf("%v: %s", err, errOut)
	}
	if out != "dati" {
		t.Fatalf("out %q", out)
	}
	if data, err := os.ReadFile(filepath.Join(rw, "res.txt")); err != nil || string(data) != "scritto\n" {
		t.Fatalf("writable mount: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(ro, "new")); !os.IsNotExist(err) {
		t.Fatal("read-only mount was written")
	}
}

func TestProtectedPathsCannotBeGranted(t *testing.T) {
	s := sandboxOrSkip(t)
	parent := t.TempDir()
	secret := filepath.Join(parent, "secrets")
	if err := os.Mkdir(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{parent, secret} {
		_, err := s.Start(context.Background(), Spec{Argv: []string{"/usr/bin/true"}, Mounts: []Mount{{Source: source, Target: "/w"}}, Protected: []string{secret}})
		if !errors.HasCode(err, ErrGrant) {
			t.Errorf("%s granted: %v", source, err)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(context.Background(), Spec{Argv: []string{"/usr/bin/true"}, Mounts: []Mount{{Source: link, Target: "/w"}}, Protected: []string{secret}}); !errors.HasCode(err, ErrGrant) {
		t.Fatalf("granted through a symlink: %v", err)
	}
}

// A mount that holds a protected path is granted when it hides it (a home
// with the agent's configuration in it): the program never sees it. In a
// writable mount the path must exist, or the program could create it.
func TestProtectedPathsHiddenInAMount(t *testing.T) {
	s := sandboxOrSkip(t)
	parent := t.TempDir()
	secret := filepath.Join(parent, "secrets")
	if err := os.Mkdir(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "k"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, writable := range []bool{false, true} {
		out, _, err := run(t, s, Spec{Argv: []string{"/bin/sh", "-c", "cat /w/secrets/k 2>&1; ls /w"}, Protected: []string{secret},
			Mounts: []Mount{{Source: parent, Target: "/w", Writable: writable, Hide: []string{"secrets"}}}})
		if err != nil || strings.Contains(out, "SECRET") || !strings.Contains(out, "secrets") {
			t.Fatalf("writable %v: %q %v", writable, out, err)
		}
	}
	missing := filepath.Join(parent, "later")
	_, err := s.Start(context.Background(), Spec{Argv: []string{"/usr/bin/true"}, Protected: []string{missing},
		Mounts: []Mount{{Source: parent, Target: "/w", Writable: true, Hide: []string{"later"}}}})
	if !errors.HasCode(err, ErrGrant) {
		t.Fatalf("a writable mount over a protected path the program could create: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created")
	}
}

func TestNoNetworkMeansNoSockets(t *testing.T) {
	s := sandboxOrSkip(t)
	out, _, err := run(t, s, Spec{Argv: []string{"/usr/bin/python3", "-c",
		"import socket\ntry:\n socket.socket()\n print('open')\nexcept OSError as e:\n print('refused', e.errno)"}})
	if err != nil || strings.TrimSpace(out) != "refused 1" {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestSeccompDeniesKernelSurface(t *testing.T) {
	s := sandboxOrSkip(t)
	numbers := map[string]string{"amd64": "323,321,298", "arm64": "282,280,241"} // userfaultfd, bpf, perf_event_open
	script := "import ctypes\nlibc = ctypes.CDLL(None, use_errno=True)\nfor n in (" + numbers[runtime.GOARCH] + ",):\n print(libc.syscall(n, 0, 0, 0, 0, 0), ctypes.get_errno())"
	out, errOut, err := run(t, s, Spec{Argv: []string{"/usr/bin/python3", "-c", script}})
	if err != nil {
		t.Fatalf("%v: %s", err, errOut)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "-1 1" {
			t.Fatalf("a denied call was not refused with EPERM: %q", out)
		}
	}
}

func TestMemoryLimit(t *testing.T) {
	s := sandboxOrSkip(t)
	out, errOut, err := run(t, s, Spec{
		Argv:   []string{"/usr/bin/python3", "-c", "try:\n x = bytearray(512*1024*1024)\n print('allocated')\nexcept MemoryError:\n print('limited')"},
		Limits: Limits{Memory: 128 << 20},
	})
	if err != nil || strings.TrimSpace(out) != "limited" {
		t.Fatalf("out %q err %v %s", out, err, errOut)
	}
}

func TestCancelKillsEverything(t *testing.T) {
	s := sandboxOrSkip(t)
	marker := "ringil-sandbox-sleep-" + strconv.Itoa(os.Getpid())
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := runCtx(ctx, t, s, Spec{Argv: sh("setsid bash -c 'exec -a " + marker + " sleep 60' & exec -a " + marker + " sleep 60")})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	time.Sleep(300 * time.Millisecond)
	procs, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	if hasProcess(procs, marker) {
		t.Fatal("a process survived the sandbox")
	}
}

func hasProcess(procs []os.DirEntry, marker string) bool {
	for _, p := range procs {
		if cmd, err := os.ReadFile("/proc/" + p.Name() + "/cmdline"); err == nil && bytes.Contains(cmd, []byte(marker)) {
			return true
		}
	}
	return false
}

func TestSpecValidation(t *testing.T) {
	s := sandboxOrSkip(t)
	bad := []Spec{
		{},
		{Argv: []string{""}},
		{Argv: []string{"x"}, Dir: "relative"},
		{Argv: []string{"x"}, Mounts: []Mount{{Source: "/tmp", Target: "relative"}}},
		{Argv: []string{"x"}, Mounts: []Mount{{Source: "/tmp", Target: "/usr/local"}}},
		{Argv: []string{"x"}, Mounts: []Mount{{Source: "/tmp", Target: "/w/../etc"}}},
		{Argv: []string{"x"}, Mounts: []Mount{{Source: "/tmp", Target: "/w"}, {Source: "/tmp", Target: "/w"}}},
		{Argv: []string{"x"}, Mounts: []Mount{{Target: "/w"}}},
		{Argv: []string{"x"}, Env: map[string]string{"PATH": "/evil"}},
		{Argv: []string{"x"}, Env: map[string]string{"A=B": "x"}},
	}
	for i, spec := range bad {
		if _, err := s.Start(context.Background(), spec); !errors.HasCode(err, ErrConfig) {
			t.Errorf("case %d accepted: %v", i, err)
		}
	}
	if _, err := s.Start(context.Background(), Spec{Argv: []string{"x"}, Mounts: []Mount{{Source: "/nonexistent-ringil", Target: "/w"}}}); !errors.HasCode(err, ErrGrant) {
		t.Fatalf("missing source: %v", err)
	}
}

// A directory granted without a target appears at its host path, so a
// request naming a real path ("create /home/me/docs/file.txt") works, and
// it works from any working directory.
func TestSamePathMountWritesOutsideTheWorkingDirectory(t *testing.T) {
	s := sandboxOrSkip(t)
	work, other := t.TempDir(), t.TempDir()
	target := filepath.Join(other, "sub", "file.txt")
	_, errOut, err := run(t, s, Spec{
		Argv:   sh(`mkdir -p "$(dirname "$T")" && echo creato > "$T" && pwd`),
		Env:    map[string]string{"T": target},
		Mounts: []Mount{{Source: work}, {Source: other, Writable: true}},
		Dir:    work,
	})
	if err != nil {
		t.Fatalf("%v: %s", err, errOut)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "creato\n" {
		t.Fatalf("file in the other folder: %q %v", data, err)
	}
}

// Audit 18/11, finding 1: a datagram socketpair could be re-aimed at a host
// Unix socket. Stream pairs stay available for event loops.
func TestSocketpairOnlyStream(t *testing.T) {
	s := sandboxOrSkip(t)
	script := "import socket\nfor ty in (socket.SOCK_STREAM, socket.SOCK_SEQPACKET, socket.SOCK_DGRAM):\n try:\n  socket.socketpair(socket.AF_UNIX, ty)\n  print('ok')\n except OSError as e:\n  print('refused', e.errno)"
	out, errOut, err := run(t, s, Spec{Argv: []string{"/usr/bin/python3", "-c", script}})
	if err != nil || strings.TrimSpace(out) != "ok\nok\nrefused 1" {
		t.Fatalf("%q %v %s", out, err, errOut)
	}
}

// Audit 18/11, finding 2: relative protected paths were ignored.
func TestRelativeProtectedRefused(t *testing.T) {
	s := sandboxOrSkip(t)
	_, err := s.Start(context.Background(), Spec{Argv: []string{"/usr/bin/true"}, Mounts: []Mount{{Source: t.TempDir()}}, Protected: []string{".ssh"}})
	if !errors.HasCode(err, ErrConfig) {
		t.Fatalf("err %v", err)
	}
}

// Audit 18/11, finding 3: a protected file not yet created, under a
// symlinked parent, could be planted from inside.
func TestMissingProtectedUnderSymlinkIsResolved(t *testing.T) {
	s := sandboxOrSkip(t)
	base := t.TempDir()
	real := filepath.Join(base, "realdata")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Start(context.Background(), Spec{Argv: []string{"/usr/bin/true"},
		Mounts: []Mount{{Source: real, Writable: true}}, Protected: []string{filepath.Join(base, "link", "secret")}})
	if !errors.HasCode(err, ErrGrant) {
		t.Fatalf("err %v", err)
	}
}

// Audit 18/11, finding 4: /dev was an unsized writable tmpfs.
func TestDevIsReadOnlyAndShmIsBounded(t *testing.T) {
	s := sandboxOrSkip(t)
	out, errOut, err := run(t, s, Spec{Argv: sh(`touch /dev/x 2>/dev/null && echo dev-writable || echo dev-ro
head -c 100M /dev/zero > /dev/shm/big 2>/dev/null && echo shm-unbounded || echo shm-bounded`)})
	if err != nil || strings.TrimSpace(out) != "dev-ro\nshm-bounded" {
		t.Fatalf("%q %v %s", out, err, errOut)
	}
}

func TestHiddenPathsAreMasked(t *testing.T) {
	s := sandboxOrSkip(t)
	dir := t.TempDir()
	for name, body := range map[string]string{".env": "SECRET=1", "ok.txt": "visibile", ".ssh/id_ed25519": "KEY"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, errOut, err := run(t, s, Spec{
		Argv:   sh(`cat /w/ok.txt; echo; cat /w/.env; ls /w/.ssh; echo end`),
		Mounts: []Mount{{Source: dir, Target: "/w", Hide: []string{".env", ".ssh", "missing"}}},
	})
	if err != nil || out != "visibile\nend\n" {
		t.Fatalf("output %q %v: %s", out, err, errOut)
	}
	_, err = s.Start(context.Background(), Spec{Argv: []string{"true"}, Mounts: []Mount{{Source: dir, Target: "/w", Hide: []string{"../etc"}}}})
	if !errors.HasCode(err, ErrConfig) {
		t.Fatalf("escaping hide accepted: %v", err)
	}
}
