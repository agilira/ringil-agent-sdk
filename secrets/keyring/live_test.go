//go:build live

// live_test.go: the system's keyring, with a service of its own
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package keyring

import (
	"context"
	"crypto/rand"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// liveKeyring is a keyring of its own service; on a Linux machine with no
// session bus (a CI runner) there is none, and the test is skipped.
func liveKeyring(t *testing.T) *Keyring {
	t.Helper()
	if _, ok := sessionBus(); !ok {
		t.Skip("no session bus: no keyring here")
	}
	k, err := New("ringil-live-test-" + rand.Text()[:8])
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// sessionBus is where a Linux keyring would be; elsewhere the system's.
func sessionBus() (string, bool) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return "", true
	}
	if a := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" {
		return a, true
	}
	_, err := os.Stat("/run/user/" + strconv.Itoa(os.Getuid()) + "/bus")
	return "", err == nil
}

func TestLiveSystemKeyring(t *testing.T) {
	k := liveKeyring(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = k.Delete(context.Background(), "LIVE_KEY") })
	if err := k.Set(ctx, "LIVE_KEY", "valore-di-prova"); err != nil {
		t.Fatal(err)
	}
	if v, found, err := k.Lookup(ctx, "LIVE_KEY"); err != nil || !found || v != "valore-di-prova" {
		t.Fatalf("%q %v %v", v, found, err)
	}
	if err := k.Delete(ctx, "LIVE_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := k.Lookup(ctx, "LIVE_KEY"); err != nil || found {
		t.Fatalf("after delete: %v %v", found, err)
	}
}

// Values come back byte for byte.
func TestLiveValuesRoundTrip(t *testing.T) {
	k := liveKeyring(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = k.Delete(context.Background(), "V") })
	for _, v := range []string{"", " spazi ", `"quote' \ back`, "a\nb\r\n", "àèìòù € 日本", "ringil-base64:x", "go-keyring-base64:x"} {
		if err := k.Set(ctx, "V", v); err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if got, found, err := k.Lookup(ctx, "V"); err != nil || !found || got != v {
			t.Fatalf("%q came back %q %v %v", v, got, found, err)
		}
	}
}
