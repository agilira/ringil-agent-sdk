//go:build linux || freebsd || netbsd || openbsd || dragonfly

// backend_unix.go: the Secret Service (GNOME Keyring, KWallet) over D-Bus
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package keyring

import (
	"maps"
	"os"
	"strconv"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	ssName      = "org.freedesktop.secrets"
	ssPath      = dbus.ObjectPath("/org/freedesktop/secrets")
	ssIface     = "org.freedesktop.Secret.Service"
	collIface   = "org.freedesktop.Secret.Collection"
	itemIface   = "org.freedesktop.Secret.Item"
	promptIface = "org.freedesktop.Secret.Prompt"
	loginPath   = dbus.ObjectPath("/org/freedesktop/secrets/collection/login")
	// schema marks the entries this package made: only an entry with
	// exactly these attributes is ours (another application's entry with
	// the same service and name, and more attributes, is not read).
	schema = "it.agilira.ringil.Secret"
	// promptWait is how long an unlock dialog waits for the user before
	// it is dismissed, freeing the queue.
	promptWait = 2 * time.Minute
)

type secretService struct{}

func system() backend { return secretService{} }

// secret is the Secret Service's (oayays): session, parameters, value,
// content type.
type secret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func attributes(service, name string) map[string]string {
	return map[string]string{"xdg:schema": schema, "service": service, "name": name}
}

// sessionAddress is the session bus, if this user has one: never started
// here (D-Bus would launch a daemon on a server without one).
func sessionAddress() (string, bool) {
	if a := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" {
		return a, true
	}
	p := "/run/user/" + strconv.Itoa(os.Getuid()) + "/bus"
	if info, err := os.Stat(p); err == nil && info.Mode()&os.ModeSocket != 0 {
		return "unix:path=" + p, true
	}
	return "", false
}

// conn is one call's connection: its own, closed after, so nothing of a
// call (a signal handler, a session) outlives it.
type conn struct {
	bus     *dbus.Conn
	owner   string // the service's unique name: signals from anyone else are ignored
	session dbus.ObjectPath
	coll    dbus.ObjectPath
}

func open() (*conn, error) {
	addr, ok := sessionAddress()
	if !ok {
		return nil, errNoKeyring
	}
	bus, err := dbus.Connect(addr)
	if err != nil {
		return nil, errNoKeyring
	}
	c := &conn{bus: bus}
	if err := c.setup(); err != nil {
		_ = bus.Close() // #nosec G104 -- the call failed already
		return nil, err
	}
	return c, nil
}

func (c *conn) setup() error {
	if err := c.bus.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, ssName).Store(&c.owner); err != nil {
		return errNoKeyring
	}
	svc := c.bus.Object(ssName, ssPath)
	var out dbus.Variant
	if err := svc.Call(ssIface+".OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&out, &c.session); err != nil {
		return errNoKeyring
	}
	if err := svc.Call(ssIface+".ReadAlias", 0, "default").Store(&c.coll); err != nil || c.coll == "/" || c.coll == "" {
		c.coll = loginPath
	}
	return nil
}

func (c *conn) close() { _ = c.bus.Close() } // #nosec G104 -- the call's answer is in

// find is our entries for service and name: exactly our attributes.
func (c *conn) find(service, name string) ([]dbus.ObjectPath, error) {
	want := attributes(service, name)
	var found []dbus.ObjectPath
	if err := c.bus.Object(ssName, c.coll).Call(collIface+".SearchItems", 0, want).Store(&found); err != nil {
		return nil, err
	}
	var ours []dbus.ObjectPath
	for _, item := range found {
		v, err := c.bus.Object(ssName, item).GetProperty(itemIface + ".Attributes")
		if err != nil {
			return nil, err
		}
		if got, ok := v.Value().(map[string]string); ok && maps.Equal(got, want) {
			ours = append(ours, item)
		}
	}
	return ours, nil
}

// unlock unlocks paths, asking the user if the keyring wants it.
func (c *conn) unlock(paths ...dbus.ObjectPath) error {
	var unlocked []dbus.ObjectPath
	var prompt dbus.ObjectPath
	if err := c.bus.Object(ssName, ssPath).Call(ssIface+".Unlock", 0, paths).Store(&unlocked, &prompt); err != nil {
		return err
	}
	_, err := c.prompt(prompt)
	return err
}

// prompt shows the keyring's dialog and waits for its answer, from the
// service alone, at most promptWait; it returns the prompt's result (for
// CreateItem, the new entry).
func (c *conn) prompt(p dbus.ObjectPath) (dbus.Variant, error) {
	if p == "/" || p == "" {
		return dbus.Variant{}, nil
	}
	match := []dbus.MatchOption{dbus.WithMatchObjectPath(p), dbus.WithMatchInterface(promptIface),
		dbus.WithMatchMember("Completed"), dbus.WithMatchSender(c.owner)}
	if err := c.bus.AddMatchSignal(match...); err != nil {
		return dbus.Variant{}, err
	}
	signals := make(chan *dbus.Signal, 8)
	c.bus.Signal(signals)
	defer c.bus.RemoveSignal(signals)
	if err := c.bus.Object(ssName, p).Call(promptIface+".Prompt", 0, "").Err; err != nil {
		return dbus.Variant{}, err
	}
	return c.answer(p, signals)
}

// answer waits for prompt p's Completed signal, at most promptWait, then
// dismisses it.
func (c *conn) answer(p dbus.ObjectPath, signals chan *dbus.Signal) (dbus.Variant, error) {
	timeout := time.NewTimer(promptWait)
	defer timeout.Stop()
	for {
		select {
		case s, ok := <-signals:
			if !ok {
				return dbus.Variant{}, errNoKeyring
			}
			if done, result, err := completed(s, p, c.owner); done {
				return result, err
			}
		case <-timeout.C:
			_ = c.bus.Object(ssName, p).Call(promptIface+".Dismiss", 0).Err // #nosec G104 -- the call fails anyway
			return dbus.Variant{}, errRefused
		}
	}
}

// completed reads a Completed signal of prompt p from owner: done, its
// result, and whether the user went on (err nil) or dismissed it.
func completed(s *dbus.Signal, p dbus.ObjectPath, owner string) (bool, dbus.Variant, error) {
	if s == nil || s.Path != p || s.Sender != owner || s.Name != promptIface+".Completed" {
		return false, dbus.Variant{}, nil
	}
	if len(s.Body) < 2 {
		return true, dbus.Variant{}, errRefused
	}
	dismissed, ok := s.Body[0].(bool)
	result, isVariant := s.Body[1].(dbus.Variant)
	if !ok || !isVariant || dismissed {
		return true, dbus.Variant{}, errRefused
	}
	return true, result, nil
}

func (secretService) get(service, name string) (string, bool, error) {
	c, err := open()
	if err != nil {
		return "", false, err
	}
	defer c.close()
	items, err := c.find(service, name)
	if err != nil || len(items) == 0 {
		return "", false, err // absent: no unlock asked
	}
	if err := c.unlock(items[0]); err != nil {
		return "", false, err
	}
	var s secret
	if err := c.bus.Object(ssName, items[0]).Call(itemIface+".GetSecret", 0, c.session).Store(&s); err != nil {
		return "", false, err
	}
	return string(s.Value), true, nil
}

func (secretService) set(service, name, value string) error {
	c, err := open()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.unlock(c.coll); err != nil {
		return err
	}
	props := map[string]dbus.Variant{
		itemIface + ".Label":      dbus.MakeVariant(service + ": " + name),
		itemIface + ".Attributes": dbus.MakeVariant(attributes(service, name)),
	}
	s := secret{Session: c.session, Value: []byte(value), ContentType: "text/plain; charset=utf8"}
	var item, prompt dbus.ObjectPath
	// Never replace=true: GNOME Keyring then replaces the first entry that
	// holds these attributes, another application's with more included
	// (measured 21/11). A new entry, then ours before it go.
	if err := c.bus.Object(ssName, c.coll).Call(collIface+".CreateItem", 0, props, s, false).Store(&item, &prompt); err != nil {
		return err
	}
	result, err := c.prompt(prompt)
	if err != nil {
		return err
	}
	if item == "/" { // made after the prompt: its result names it
		made, ok := result.Value().(dbus.ObjectPath)
		if !ok || made == "/" {
			return errNoKeyring // never delete the entries before without the new one
		}
		item = made
	}
	return c.deleteOthers(service, name, item)
}

// deleteOthers removes our entries for name but keep: one name, one
// entry, whatever an earlier run left.
func (c *conn) deleteOthers(service, name string, keep dbus.ObjectPath) error {
	items, err := c.find(service, name)
	if err != nil {
		return err
	}
	var others []dbus.ObjectPath
	for _, item := range items {
		if item != keep {
			others = append(others, item)
		}
	}
	if len(others) == 0 {
		return nil // nothing to delete: no unlock asked
	}
	if err := c.unlock(c.coll); err != nil { // a locked entry cannot be deleted
		return err
	}
	for _, item := range others {
		if err := c.deleteItem(item); err != nil {
			return err
		}
	}
	return nil
}

func (c *conn) deleteItem(item dbus.ObjectPath) error {
	var prompt dbus.ObjectPath
	if err := c.bus.Object(ssName, item).Call(itemIface+".Delete", 0).Store(&prompt); err != nil {
		return err
	}
	_, err := c.prompt(prompt)
	return err
}

func (secretService) del(service, name string) error {
	c, err := open()
	if err != nil {
		return err
	}
	defer c.close()
	return c.deleteOthers(service, name, "")
}
