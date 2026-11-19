// groups_test.go: group chats and membership
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package telegram

import (
	"context"
	"testing"
)

func TestGroupMessages(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	b.set("getUpdates", `{"ok":true,"result":[
		{"update_id":1,"message":{"chat":{"id":-5,"type":"group"},"from":{"id":7},"text":"lunch?"}},
		{"update_id":2,"message":{"chat":{"id":-5,"type":"supergroup"},"from":{"id":7},"text":"@testbot what time is it"}},
		{"update_id":3,"message":{"chat":{"id":-5,"type":"group"},"from":{"id":7},"text":"thanks","reply_to_message":{"chat":{"id":-5},"from":{"id":1}}}},
		{"update_id":4,"message":{"chat":{"id":-5,"type":"group"},"from":{"id":7},"text":"/new"}},
		{"update_id":5,"message":{"chat":{"id":-5,"type":"group"},"from":{"id":7},"text":"/new@OtherBot"}},
		{"update_id":6,"message":{"chat":{"id":-5,"type":"group"},"from":{"id":7},"text":"/new@TestBot"}},
		{"update_id":7,"message":{"chat":{"id":-5,"type":"group"},"from":{"id":7},"text":"mail @TestBotFan"}},
		{"update_id":8,"message":{"chat":{"id":7,"type":"private"},"from":{"id":7},"text":"hi"}}]}`,
		`{"ok":true,"result":[]}`)
	want := []struct {
		text             string
		group, addressed bool
	}{
		{"lunch?", true, false}, {"what time is it", true, true}, {"thanks", true, true}, {"/new", true, true},
		{"/new@OtherBot", true, false}, {"/new", true, true}, {"mail @TestBotFan", true, false}, {"hi", false, false},
	}
	for i, w := range want {
		m, err := c.Receive(context.Background())
		if err != nil || m.Text != w.text || m.Group != w.group || m.Addressed != w.addressed {
			t.Errorf("%d: %q group %v addressed %v %v", i, m.Text, m.Group, m.Addressed, err)
		}
	}
}

func TestMemberOf(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	in := c.MemberOf("-100")
	for status, want := range map[string]bool{
		`{"status":"member"}`: true, `{"status":"administrator"}`: true, `{"status":"creator"}`: true,
		`{"status":"restricted","is_member":true}`: true, `{"status":"restricted","is_member":false}`: false,
		`{"status":"left"}`: false, `{"status":"kicked"}`: false,
	} {
		b.set("getChatMember", `{"ok":true,"result":`+status+`}`)
		if got, err := in(context.Background(), "7"); err != nil || got != want {
			t.Errorf("%s: %v %v", status, got, err)
		}
	}
	if a := b.called("getChatMember"); a[0]["chat_id"] != "-100" || a[0]["user_id"] != "7" {
		t.Fatalf("asked %v", a[0])
	}
	b.set("getChatMember", `{"ok":false,"description":"Bad Request: chat not found"}`)
	if _, err := in(context.Background(), "7"); err == nil {
		t.Fatal("an error taken as an answer")
	}
	if _, err := in(context.Background(), "x/../y"); err == nil {
		t.Fatal("not an ID")
	}
}

func TestSharedSendersAreNobody(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	b.set("getUpdates", `{"ok":true,"result":[
		{"update_id":1,"message":{"chat":{"id":-5,"type":"supergroup"},"from":{"id":1087968824},"sender_chat":{"id":-5},"text":"@TestBot hi"}},
		{"update_id":2,"message":{"chat":{"id":-5,"type":"supergroup"},"from":{"id":777000},"text":"@TestBot hi"}},
		{"update_id":3,"message":{"chat":{"id":-5,"type":"supergroup"},"from":{"id":9},"sender_chat":{"id":-77},"text":"@TestBot hi"}}]}`,
		`{"ok":true,"result":[]}`)
	for i := range 3 {
		if m, err := c.Receive(context.Background()); err != nil || m.From != "" {
			t.Errorf("%d: from %q %v", i, m.From, err)
		}
	}
}
