// people_test.go: who a Bridge talks with, changed while it runs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetPeopleTakesEffectAtTheNextMessage(t *testing.T) {
	b, app, _, events := teamBridge(t, Config{})
	say(b, worker, worker, "hi")
	if e := <-events; e.Kind != Refused {
		t.Fatalf("not yet a member: %+v", e)
	}
	if err := b.SetPeople(People{Owners: []string{me}, Members: []string{worker}, MemberProfile: "team"}); err != nil {
		t.Fatal(err)
	}
	say(b, worker, worker, "hi")
	if got := app.nextIn(t, worker).Text; got != "team" {
		t.Fatalf("a member added: %q", got)
	}
	if err := b.SetPeople(People{Owners: []string{worker}}); err != nil {
		t.Fatal(err)
	}
	say(b, me, me, "hi")
	for e := <-events; e.Kind != Refused; e = <-events {
	}
	say(b, worker, worker, "hi again")
	if got := app.nextIn(t, worker).Text; got != "default" {
		t.Fatalf("made an owner: %q", got)
	}
}

func TestSetPeopleRefusedKeepsThePrevious(t *testing.T) {
	b, app, _, _ := teamBridge(t, Config{Members: []string{worker}, MemberProfile: "team"})
	if err := b.SetPeople(People{Owners: []string{me}, Members: []string{worker}}); err == nil {
		t.Fatal("members without their profile were accepted")
	}
	if err := b.SetPeople(People{Owners: []string{me}, MemberPhones: []string{"+39 333 1234567"}, MemberProfile: "team"}); err == nil {
		t.Fatal("phones on a channel that cannot ask for them were accepted")
	}
	say(b, worker, worker, "hi")
	if got := app.nextIn(t, worker).Text; got != "team" {
		t.Fatalf("the refused people were applied: %q", got)
	}
}

func TestSetPeopleRevokesPhones(t *testing.T) {
	file := filepath.Join(t.TempDir(), "phones.json")
	b, app, _ := phoneBridge(t, file, "+39 333 1234567")
	share(b, "300", "+39 333 1234567")
	app.nextIn(t, "300") // welcome
	say(b, "300", "300", "hi")
	if got := app.nextIn(t, "300").Text; got != "team" {
		t.Fatalf("a member by phone: %q", got)
	}
	if err := b.SetPeople(People{Owners: []string{me}, MemberProfile: "team", MemberPhones: []string{"+39 333 9999999"}}); err != nil {
		t.Fatal(err)
	}
	if b.roleOf(t.Context(), "300") == member {
		t.Fatal("a phone taken off the list still let its sender in")
	}
	data, err := os.ReadFile(file)
	if err != nil || strings.Contains(string(data), "393331234567") {
		t.Fatalf("the revoked phone stayed on disk: %s %v", data, err)
	}
}

// Audit 20/11: turning the approvals back on while serve ran did not reach
// the chats.
func TestSetPeopleTurnsApprovalsBackOn(t *testing.T) {
	b, _, _, _ := teamBridge(t, Config{NoApprovals: true})
	if !b.people().noApprovals {
		t.Fatal("NoApprovals of the Config not taken")
	}
	if err := b.SetPeople(People{Owners: []string{me}}); err != nil {
		t.Fatal(err)
	}
	if b.people().noApprovals {
		t.Fatal("the approvals stayed off")
	}
}
