// format_test.go: the agent's markdown as Telegram shows it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package telegram

import (
	"context"
	"strings"
	"testing"

	"github.com/agilira/ringil-agent-sdk/channel"
)

func TestFormat(t *testing.T) {
	for in, want := range map[string]string{
		"**Percorso:** `/home/a/prova.txt`":         "<b>Percorso:</b> <code>/home/a/prova.txt</code>",
		"## Fatto\n- uno\n- *due*":                  "<b>Fatto</b>\n• uno\n• <i>due</i>",
		"a <b>x</b> & y":                            "a &lt;b&gt;x&lt;/b&gt; &amp; y",
		"```go\nif a < b {}\n```\nok":               "<pre>if a &lt; b {}</pre>\nok",
		"`**not bold**`":                            "<code>**not bold**</code>",
		"[site](https://x.org/?a=1&b=\"2\") ~~no~~": `<a href="https://x.org/?a=1&amp;b=&#34;2&#34;">site</a> <s>no</s>`,
		"[bad](javascript:alert(1))":                "[bad](javascript:alert(1))",
		"snake_case_name and 2*3*4":                 "snake_case_name and 2*3*4",
		"```\nunclosed":                             "```\nunclosed",
	} {
		if got := format(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestSendMarkdown(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	if err := c.Send(context.Background(), "1", channel.Out{Text: "**ok**", Markdown: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background(), "1", channel.Out{Text: "**approve**"}); err != nil {
		t.Fatal(err)
	}
	s := b.called("sendMessage")
	if s[0]["text"] != "<b>ok</b>" || s[0]["parse_mode"] != "HTML" {
		t.Fatalf("markdown: %v", s[0])
	}
	if s[1]["text"] != "**approve**" || s[1]["parse_mode"] != nil {
		t.Fatalf("plain: %v", s[1])
	}
}

func TestBadFormattingGoesPlain(t *testing.T) {
	b, srv := newBotAPI(t)
	b.set("sendMessage", `{"ok":false,"description":"Bad Request: can't parse entities: unexpected end tag"}`, `{"ok":true,"result":{}}`)
	c := open(t, srv.URL)
	if err := c.Send(context.Background(), "1", channel.Out{Text: "**a *b** c*", Markdown: true}); err != nil {
		t.Fatal(err)
	}
	s := b.called("sendMessage")
	if len(s) != 2 || s[1]["text"] != "**a *b** c*" || s[1]["parse_mode"] != nil {
		t.Fatalf("%v", s)
	}
}

func TestSendCode(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	err := c.Send(context.Background(), "1", channel.Out{Text: "May I write a<b?", Code: "x < y **z**", Buttons: []channel.Button{{ID: "t:y", Label: "Yes"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := b.called("sendMessage")
	if len(s) != 1 || s[0]["text"] != "May I write a&lt;b?\n<pre>x &lt; y **z**</pre>" || s[0]["parse_mode"] != "HTML" || s[0]["reply_markup"] == nil {
		t.Fatalf("%v", s)
	}
}

func TestLongCodeGoesAsText(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	code := strings.Repeat("line <x>\n", 1000)
	if err := c.Send(context.Background(), "1", channel.Out{Text: "May I?", Code: code, Buttons: []channel.Button{{ID: "t:y", Label: "Yes"}}}); err != nil {
		t.Fatal(err)
	}
	s := b.called("sendMessage")
	if len(s) < 2 || s[0]["parse_mode"] != nil || !strings.HasPrefix(s[0]["text"].(string), "May I?\n\nline <x>") || s[len(s)-1]["reply_markup"] == nil {
		t.Fatalf("%d parts: %v", len(s), s[0])
	}
}
