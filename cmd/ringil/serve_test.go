// serve_test.go: the API token of ringil serve
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agilira/orpheus/pkg/orpheus"
)

func TestServeToken(t *testing.T) {
	dir := t.TempDir()
	c := newCLI(strings.NewReader(""), io.Discard, io.Discard)
	c.getenv = func(string) string { return "" }
	c.configDir = func() (string, error) { return dir, nil }
	c.dataDir = func() (string, error) { return filepath.Join(dir, "data"), nil }
	first, where, err := c.keptServeToken()
	if err != nil || len(first) < 32 || !strings.Contains(where, "(new)") {
		t.Fatalf("%q %q %v", first, where, err)
	}
	again, _, err := c.keptServeToken()
	if err != nil || again != first {
		t.Fatalf("not kept: %v", err)
	}
	if runtime.GOOS != "windows" {
		checkPrivateToken(t, c, filepath.Join(dir, "ringil", tokenFile))
	}
}

func TestServeTokenFromEnv(t *testing.T) {
	c := newCLI(strings.NewReader(""), io.Discard, io.Discard)
	c.getenv = func(k string) string {
		if k == "RINGIL_SERVE_TOKEN" {
			return "from-env"
		}
		return ""
	}
	probe(t, c, nil, func(ctx *orpheus.Context) error {
		if tok, where, _ := c.serveToken(ctx); tok != "from-env" || !strings.Contains(where, "RINGIL_SERVE_TOKEN") {
			t.Fatalf("%q %q", tok, where)
		}
		return nil
	})
}

// checkPrivateToken: a token file others can read is refused.
func checkPrivateToken(t *testing.T, c *cli, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.keptServeToken(); err == nil {
		t.Fatal("a token readable by others was used")
	}
}

// In a container serve listens on every interface by default, and the
// link and the log name the loopback the host reaches it on.
func TestServeInAContainer(t *testing.T) {
	h := newHarness(t, "")
	if h.c.inContainer() || h.c.serveListen() != defaultListen {
		t.Fatal("a container outside one")
	}
	env(h, map[string]string{"RINGIL_CONTAINER": "1"})
	h.c.inImage = func() bool { return false }
	if h.c.inContainer() {
		t.Fatal("the variable alone, on a desktop, made it a container")
	}
	h.c.inImage = func() bool { return true }
	if !h.c.inContainer() || h.c.serveListen() != containerListen {
		t.Fatalf("in a container: %v %s", h.c.inContainer(), h.c.serveListen())
	}
	checkContainerAddresses(t)
	for in, want := range map[string]string{"0.0.0.0:8765": "127.0.0.1:8765", "[::]:9": "127.0.0.1:9", "127.0.0.1:1": "127.0.0.1:1", ":7": "127.0.0.1:7"} {
		if got := loopbackOf(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// checkContainerAddresses: the link goes to this machine's loopback, the
// host is the route table's gateway.
func checkContainerAddresses(t *testing.T) {
	t.Helper()
	if got := localOf("10.0.0.9:9999"); got != "127.0.0.1:9999" {
		t.Fatalf("the link would go to %s", got)
	}
	table := "Iface\tDestination\tGateway\tFlags\neth0\t00000000\t010011AC\t0003\neth0\t000011AC\t00000000\t0001\n"
	if gw, ok := gatewayOf(table); !ok || gw.String() != "172.17.0.1" {
		t.Fatalf("gateway %v %v", gw, ok)
	}
}

func TestWantsBrowserRespectsFlagCombination(t *testing.T) {
	c := newCLI(strings.NewReader(""), io.Discard, io.Discard)
	c.getenv = func(string) string { return "" }
	c.inImage = func() bool { return false }
	var gotTerm, gotNonTerm bool
	app := orpheus.New("ringil").AddCommand(orpheus.NewCommand("serve", "s").
		AddBoolFlag("no-browser", "", false, "nb").
		AddBoolFlag("open", "", false, "op").
		SetHandler(func(ctx *orpheus.Context) error {
			gotTerm = c.wantsBrowser(ctx, true)
			gotNonTerm = c.wantsBrowser(ctx, false)
			return nil
		}))
	if err := app.Run([]string{"serve", "--open", "--no-browser"}); err != nil {
		t.Fatal(err)
	}
	if gotTerm || gotNonTerm {
		t.Fatalf("wantsBrowser ignored --no-browser when --open was set (%v, %v)", gotTerm, gotNonTerm)
	}
}

// Audit 27/11: the link goes to serve's own loopback, ::1 too.
func TestLocalOfKeepsTheLoopback(t *testing.T) {
	for in, want := range map[string]string{"[::1]:9": "[::1]:9", "127.0.0.2:9": "127.0.0.2:9", "[::]:9": "[::1]:9",
		"0.0.0.0:9": "127.0.0.1:9", ":9": "127.0.0.1:9", "localhost:9": "127.0.0.1:9", "[::ffff:127.0.0.1]:9": "127.0.0.1:9"} {
		if got := localOf(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// Audit 27/11: a start that fails once serve listens leaves the port free.
func TestServeFailedStartFreesThePort(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	h := newHarness(t, "")
	writeConfig(t, h.dir, "models: [openai:m]\nserve:\n  listen: "+addr+"\n")
	writeState(t, filepath.Join(h.dir, "data", "inbox"), "a file, not a folder") // the chats' inbox cannot be made
	if code := h.run("serve"); code == exitOK || !strings.Contains(h.err.String(), "inbox") {
		t.Fatalf("serve %d: %s", code, h.err)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port is still taken: %v", err)
	}
	_ = l.Close()
}
