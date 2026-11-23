// security_test.go: cross-platform validation and path boundary tests for sandbox
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package sandbox

import (
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/agilira/go-errors"
)

func TestSecurityValidateSpecMatrix(t *testing.T) {
	src := t.TempDir()
	assertSpecBad := func(name string, s Spec) {
		t.Helper()
		if err := validate(s); !errors.HasCode(err, ErrConfig) {
			t.Errorf("%s: accepted or wrong error: %v", name, err)
		}
	}

	for _, target := range append(append([]string{}, reservedTargets...), "/usr/local", "/etc/passwd", "/proc/self") {
		assertSpecBad("reserved:"+target, Spec{Argv: []string{"true"}, Mounts: []Mount{{Source: src, Target: target}}})
	}
	if err := validate(Spec{Argv: []string{"true"}, Mounts: []Mount{{Source: src, Target: "/tmp/work"}}}); err != nil {
		t.Errorf("/tmp/work refused: %v", err)
	}

	assertInvalidHomeAndPath(t, src, assertSpecBad)
	assertInvalidProxyAndEnv(t, src, assertSpecBad)
	assertInvalidHideAndReadOnly(t, src, assertSpecBad)
}

func assertInvalidHomeAndPath(t *testing.T, src string, assertBad func(string, Spec)) {
	t.Helper()
	for _, h := range []string{"rel/home", "/a/../b", "/etc", "/tmp", "/w", "/w/sub"} {
		assertBad("home:"+h, Spec{Argv: []string{"true"}, Home: h, Mounts: []Mount{{Source: src, Target: "/w"}}})
	}
	for _, p := range []string{"rel/bin", "/usr/bin:/bin", "/usr/bin\x00"} {
		assertBad("path:"+p, Spec{Argv: []string{"true"}, Path: []string{p}})
	}
}

func assertInvalidProxyAndEnv(t *testing.T, src string, assertBad func(string, Spec)) {
	t.Helper()
	sock := filepath.Join(src, "proxy.sock")
	assertBad("proxiedNoSock", Spec{Argv: []string{"true"}, Network: ProxiedNetwork})
	assertBad("noNetWithSock", Spec{Argv: []string{"true"}, Network: NoNetwork, Proxy: sock})
	assertBad("relProxy", Spec{Argv: []string{"true"}, Network: ProxiedNetwork, Proxy: "proxy.sock"})
	assertBad("unknownNet", Spec{Argv: []string{"true"}, Network: Network(7)})

	badEnv := map[string]string{
		"empty":  "",
		"equals": "A=B",
		"nulKey": "A\x00B",
		"PATH":   "PATH",
		"HOME":   "HOME",
		"TMPDIR": "TMPDIR",
		"LANG":   "LANG",
	}
	for name, k := range badEnv {
		assertBad("env:"+name, Spec{Argv: []string{"true"}, Env: map[string]string{k: "x"}})
	}
	assertBad("env:nulVal", Spec{Argv: []string{"true"}, Env: map[string]string{"OK": "x\x00y"}})
	for _, k := range proxyEnv {
		assertBad("env:"+k, Spec{Argv: []string{"true"}, Env: map[string]string{k: "http://evil"}})
	}
}

func assertInvalidHideAndReadOnly(t *testing.T, src string, assertBad func(string, Spec)) {
	t.Helper()
	for _, h := range []string{".", "..", "../x", "/abs", "a/../b", "a//b", ""} {
		assertBad("hide:"+h, Spec{Argv: []string{"true"}, Mounts: []Mount{{Source: src, Target: "/w", Hide: []string{h}}}})
		assertBad("ro:"+h, Spec{Argv: []string{"true"}, Mounts: []Mount{{Source: src, Target: "/w", Writable: true, ReadOnly: []string{h}}}})
	}
	assertBad("roNotWritable", Spec{Argv: []string{"true"}, Mounts: []Mount{{Source: src, Target: "/w", ReadOnly: []string{"sub"}}}})
	tooMany := make([]string, MaxHidden+1)
	for i := range tooMany {
		tooMany[i] = "f" + strconv.Itoa(i)
	}
	assertBad("tooManyHidden", Spec{Argv: []string{"true"}, Mounts: []Mount{{Source: src, Target: "/w", Hide: tooMany}}})
}

func TestSecurityResolveAndWithin(t *testing.T) {
	if _, err := resolve("relative/path"); !errors.HasCode(err, ErrConfig) {
		t.Fatalf("relative path accepted by resolve: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	base := t.TempDir()
	userDir := filepath.Join(base, "user")
	if within(filepath.Join(base, "user2"), userDir) {
		t.Error("within matched sibling prefix collision")
	}
	if within(filepath.Join(userDir, "..", "other"), userDir) {
		t.Error("within matched parent escape")
	}
	if !within(filepath.Join(userDir, "sub", "file"), userDir) {
		t.Error("within rejected child path")
	}
}
