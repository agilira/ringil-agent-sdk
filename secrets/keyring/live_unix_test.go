//go:build live && (linux || freebsd || netbsd || openbsd || dragonfly)

// live_unix_test.go: another application's entry is not ours
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package keyring

import (
	"context"
	"testing"

	"github.com/godbus/dbus/v5"
)

// foreignEntry stores, as another application would, an entry with our
// attributes and one more; it returns a function reading it back, and
// removes it at the end.
func foreignEntry(t *testing.T, service, name, value string) func() (string, bool) {
	t.Helper()
	c, err := open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.close)
	attrs := attributes(service, name)
	attrs["application"] = "other"
	if err := c.unlock(c.coll); err != nil {
		t.Fatal(err)
	}
	props := map[string]dbus.Variant{itemIface + ".Label": dbus.MakeVariant("foreign"), itemIface + ".Attributes": dbus.MakeVariant(attrs)}
	var item, prompt dbus.ObjectPath
	s := secret{Session: c.session, Value: []byte(value), ContentType: "text/plain"}
	if err := c.bus.Object(ssName, c.coll).Call(collIface+".CreateItem", 0, props, s, true).Store(&item, &prompt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.deleteItem(item) })
	return func() (string, bool) {
		var got secret
		if err := c.bus.Object(ssName, item).Call(itemIface+".GetSecret", 0, c.session).Store(&got); err != nil {
			return "", false
		}
		return string(got.Value), true
	}
}

// Audit 21/11: an entry of another application with our service, our
// name and one attribute more was read in place of ours, and Delete
// left it. Ours are exactly our attributes.
func TestLiveAnotherApplicationsEntry(t *testing.T) {
	k := liveKeyring(t)
	service := k.service
	ctx := context.Background()
	theirs := foreignEntry(t, service, "KEY", "FOREIGN")
	if v, found, err := k.Lookup(ctx, "KEY"); err != nil || found {
		t.Fatalf("another application's entry was read: %q %v", v, err)
	}
	if err := k.Set(ctx, "KEY", "ours"); err != nil {
		t.Fatal(err)
	}
	if v, _, err := k.Lookup(ctx, "KEY"); err != nil || v != "ours" {
		t.Fatalf("%q %v", v, err)
	}
	deleteTwice(t, k, "KEY")
	if v, ok := theirs(); !ok || v != "FOREIGN" {
		t.Fatalf("the other application's entry changed: %q %v", v, ok)
	}
}
