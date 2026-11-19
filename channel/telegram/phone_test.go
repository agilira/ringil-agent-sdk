// phone_test.go: asking a user for their phone number
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package telegram

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agilira/ringil-agent-sdk/channel"
)

func receiveOne(t *testing.T, b *botAPI, c *Channel, update string) channel.Message {
	t.Helper()
	b.set("getUpdates", `{"ok":true,"result":[`+update+`]}`, `{"ok":true,"result":[]}`)
	m, err := c.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOwnContactIsThePhone(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	m := receiveOne(t, b, c, `{"update_id":1,"message":{"chat":{"id":42,"type":"private"},"from":{"id":42},"contact":{"phone_number":"393331234567","user_id":42}}}`)
	if m.Phone != "393331234567" || m.From != "42" {
		t.Fatalf("%+v", m)
	}
}

func TestOthersContactsAreNotThePhone(t *testing.T) {
	for name, u := range map[string]string{
		"someone else's": `{"update_id":1,"message":{"chat":{"id":42,"type":"private"},"from":{"id":42},"contact":{"phone_number":"393331234567","user_id":43}}}`,
		"no user":        `{"update_id":1,"message":{"chat":{"id":42,"type":"private"},"from":{"id":42},"contact":{"phone_number":"393331234567"}}}`,
		"in a group":     `{"update_id":1,"message":{"chat":{"id":-5,"type":"group"},"from":{"id":42},"contact":{"phone_number":"393331234567","user_id":42}}}`,
	} {
		b, srv := newBotAPI(t)
		c := open(t, srv.URL)
		if m := receiveOne(t, b, c, u); m.Phone != "" {
			t.Errorf("%s: %q", name, m.Phone)
		}
	}
}

func TestAskPhoneThenTheKeyboardGoes(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	if err := c.AskPhone(context.Background(), "42", "share it", "Share my phone number"); err != nil {
		t.Fatal(err)
	}
	receiveOne(t, b, c, `{"update_id":1,"message":{"chat":{"id":42,"type":"private"},"from":{"id":42},"contact":{"phone_number":"393331234567","user_id":42}}}`)
	for range 2 {
		if err := c.Send(context.Background(), "42", channel.Out{Text: "thanks"}); err != nil {
			t.Fatal(err)
		}
	}
	s := b.called("sendMessage")
	if len(s) != 3 {
		t.Fatalf("%d sent", len(s))
	}
	ask, _ := json.Marshal(s[0]["reply_markup"])
	if string(ask) != `{"keyboard":[[{"request_contact":true,"text":"Share my phone number"}]],"one_time_keyboard":true,"resize_keyboard":true}` {
		t.Fatalf("ask %s", ask)
	}
	gone, _ := json.Marshal(s[1]["reply_markup"])
	if string(gone) != `{"remove_keyboard":true}` {
		t.Fatalf("not removed: %s", gone)
	}
	if _, has := s[2]["reply_markup"]; has {
		t.Fatal("removed twice")
	}
}

func TestLanguageOfTheSender(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	m := receiveOne(t, b, c, `{"update_id":1,"message":{"chat":{"id":42,"type":"private"},"from":{"id":42,"language_code":"it"},"text":"ciao"}}`)
	if m.Language != "it" {
		t.Fatalf("%+v", m)
	}
}
