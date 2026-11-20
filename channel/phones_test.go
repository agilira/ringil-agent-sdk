// phones_test.go: members let in by the phone number they share
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// phoneApp is a chatApp that can ask for phone numbers: what it asks
// comes out as a text starting with "ASK ".
type phoneApp struct{ *chatApp }

func (a phoneApp) AskPhone(ctx context.Context, chat, text, _ string) error {
	return a.Send(ctx, chat, Out{Text: "ASK " + text})
}

func phoneBridge(t *testing.T, file string, phones ...string) (*Bridge, phoneApp, chan Event) {
	t.Helper()
	p, app, events := newProfiled(t), phoneApp{newApp()}, make(chan Event, 16)
	b, err := NewBridge(Config{Channel: app, Server: p.srv, Owners: []string{me}, MemberProfile: "team",
		MemberPhones: phones, SharedPhones: file, OnEvent: func(e Event) { events <- e }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.wg.Wait)
	return b, app, events
}

func share(b *Bridge, from, phone string) {
	b.handle(context.Background(), Message{Chat: from, From: from, Phone: phone})
}

func TestNormalPhone(t *testing.T) {
	for in, want := range map[string]string{
		"+39 333 123 4567": "393331234567", "00 39 333": "", "0039 333-1234567": "393331234567", "393331234567": "393331234567",
		"+1 (555) 010.9999": "15550109999", "0333 1234567": "", "+39 333": "", "+39abc4567890": "", "+1234567890123456": "",
	} {
		got, ok := normalPhone(in, false)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
}

func TestListedPhonesHaveTheirCountryCode(t *testing.T) {
	if _, ok := normalPhone("333 1234567", true); ok {
		t.Error("taken without the country code")
	}
	if got, ok := normalPhone("0039 333 1234567", true); !ok || got != "393331234567" {
		t.Errorf("%q", got)
	}
}

func TestPhoneConfig(t *testing.T) {
	p := newProfiled(t)
	for name, cfg := range map[string]Config{
		"bad number": {Channel: phoneApp{newApp()}, Server: p.srv, MemberProfile: "team", MemberPhones: []string{"333 1234567"}},
		"cannot ask": {Channel: newApp(), Server: p.srv, MemberProfile: "team", MemberPhones: []string{"+393331234567"}},
		"no profile": {Channel: phoneApp{newApp()}, Server: p.srv, MemberPhones: []string{"+393331234567"}},
	} {
		if _, err := NewBridge(cfg); err == nil {
			t.Error(name)
		}
	}
}

func TestStrangerSharesAListedPhone(t *testing.T) {
	file := filepath.Join(t.TempDir(), "phones.json")
	b, app, events := phoneBridge(t, file, "+39 333 123 4567")
	say(b, outsider, outsider, "hi")
	if e := <-events; e.Kind != Refused {
		t.Fatalf("%+v", e)
	}
	if got := app.nextIn(t, outsider).Text; !strings.HasPrefix(got, "ASK ") {
		t.Fatalf("not asked: %q", got)
	}
	share(b, outsider, "393331234567")
	if got := app.nextIn(t, outsider).Text; !strings.Contains(got, "write to me now") {
		t.Fatalf("not let in: %q", got)
	}
	say(b, outsider, outsider, "hi")
	if got := app.nextIn(t, outsider).Text; got != "team" {
		t.Fatalf("member: %q", got)
	}
	b.wg.Wait()

	again, app2, _ := phoneBridge(t, file, "+393331234567") // a restart keeps them
	say(again, outsider, outsider, "hi")
	if got := app2.nextIn(t, outsider).Text; got != "team" {
		t.Fatalf("after a restart: %q", got)
	}
	again.wg.Wait()

	off, app3, events3 := phoneBridge(t, file, "+393339999999") // taken off the list
	say(off, outsider, outsider, "hi")
	if e := <-events3; e.Kind != Refused {
		t.Fatalf("still a member: %+v", e)
	}
	if got := app3.nextIn(t, outsider).Text; !strings.HasPrefix(got, "ASK ") {
		t.Fatalf("not asked: %q", got)
	}
}

func TestUnlistedPhoneIsRefused(t *testing.T) {
	b, app, events := phoneBridge(t, "", "+393331234567")
	share(b, outsider, "+393330000000")
	if e := <-events; e.Kind != Refused || e.From != outsider {
		t.Fatalf("%+v", e)
	}
	if got := app.nextIn(t, outsider).Text; !strings.Contains(got, "not among") {
		t.Fatalf("%q", got)
	}
	say(b, outsider, outsider, "hi")
	<-events
	if got := app.nextIn(t, outsider).Text; !strings.HasPrefix(got, "ASK ") {
		t.Fatalf("not a stranger: %q", got)
	}
}

func TestNoPhoneAskedInGroups(t *testing.T) {
	b, app, events := phoneBridge(t, "", "+393331234567")
	b.handle(context.Background(), Message{Chat: office, From: outsider, Group: true, Addressed: true, Text: "hi"})
	<-events
	b.handle(context.Background(), Message{Chat: office, From: outsider, Group: true, Addressed: true, Phone: "+393331234567"})
	<-events
	app.quiet(t)
}

func TestStrangersAreAskedWithinTheHourlyBound(t *testing.T) {
	b, app, _ := phoneBridge(t, "", "+393331234567")
	p := PeopleOf(b.cfg)
	p.MaxPerHour = 2
	if err := b.SetPeople(p); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		say(b, outsider, outsider, "hi")
	}
	app.nextIn(t, outsider)
	app.nextIn(t, outsider)
	app.quiet(t)
}

func TestDamagedPhonesFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "phones.json")
	if err := os.WriteFile(file, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newProfiled(t)
	_, err := NewBridge(Config{Channel: phoneApp{newApp()}, Server: p.srv, MemberProfile: "team", MemberPhones: []string{"+393331234567"}, SharedPhones: file})
	if err == nil {
		t.Fatal("damaged file read")
	}
}

func TestOwnersSharingTheirPhoneStayOwners(t *testing.T) {
	b, app, events := phoneBridge(t, "", "+393331234567")
	share(b, me, "+393330000000")
	if got := app.nextIn(t, me).Text; strings.Contains(got, "not among") {
		t.Fatalf("owner refused: %q", got)
	}
	select {
	case e := <-events:
		t.Fatalf("%+v", e)
	default:
	}
}

func TestPublicBotAsksAtStart(t *testing.T) {
	p, app := newProfiled(t), phoneApp{newApp()}
	b, err := NewBridge(Config{Channel: app, Server: p.srv, MemberProfile: "team", PublicProfile: "public", MemberPhones: []string{"+393331234567"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.wg.Wait)
	say(b, outsider, outsider, "/start")
	app.nextIn(t, outsider)
	if got := app.nextIn(t, outsider).Text; !strings.HasPrefix(got, "ASK ") {
		t.Fatalf("not asked: %q", got)
	}
	share(b, outsider, "+393331234567")
	app.nextIn(t, outsider)
	say(b, outsider, outsider, "hi")
	if got := app.nextIn(t, outsider).Text; got != "team" {
		t.Fatalf("%q", got)
	}
}

func TestRevokedPhonesLeaveTheFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "phones.json")
	if err := os.WriteFile(file, []byte(`{"300":"393331234567"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	phoneBridge(t, file, "+393339999999")
	data, err := os.ReadFile(file)
	if err != nil || strings.Contains(string(data), "393331234567") {
		t.Fatalf("%s %v", data, err)
	}
}

func TestTrunkZeroIsRefusedInTheList(t *testing.T) {
	if _, ok := normalPhone("+44 (0)20 7946 0018", true); ok {
		t.Fatal("taken")
	}
}
