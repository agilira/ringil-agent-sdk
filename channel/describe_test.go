// describe_test.go: an approval, asked as a person would ask it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"strconv"
	"strings"
	"testing"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func TestDescribe(t *testing.T) {
	it := texts[Italian]
	for _, c := range []struct{ tool, in, text, code string }{
		{"write_file", `{"path":"/d/hello.py","content":"print(1)"}`, "Posso scrivere questo in /d/hello.py?", "print(1)"},
		{"edit_file", `{"path":"a.go","old_string":"x","new_string":"y"}`, "Posso modificare a.go? Le righe con - escono, quelle con + entrano:", "- x\n+ y"},
		{"bash_unsandboxed", `{"command":"ls ~","reason":"unsandboxed","why":null,"explanation":"to see home"}`,
			"Posso eseguire questo comando?\nGira fuori dalla sandbox, con i tuoi permessi.\nL'agente dice: to see home", "ls ~"},
		{"network_domain", `{"domain":"pypi.org"}`, "Un comando vuole raggiungere pypi.org. Glielo permetto, per questa conversazione?", ""},
		{"write_file", `{"path":"a","content":"x","mode":"0777"}`, "Posso usare write_file?", "path: a\ncontent: x\nmode: 0777"},
		{"write_file", `{"path":3,"content":"x"}`, "Posso usare write_file?", "path: 3\ncontent: x"},
		{"publish", `{}`, "Posso usare publish?", ""},
	} {
		q := describe(it, c.tool, []byte(c.in))
		if q.text != c.text || q.code != c.code {
			t.Errorf("%s %s:\n got %q / %q\nwant %q / %q", c.tool, c.in, q.text, q.code, c.text, c.code)
		}
	}
}

func TestPlaceholdersInArgsStay(t *testing.T) {
	if got := texts[English].t(mTooLarge, "{}", "5"); got != "{} is too large: up to 5 MB." {
		t.Fatal(got)
	}
}

func TestYesAlways(t *testing.T) {
	r := newRig(t, true, fake.Call("delete_file", `{}`), fake.Reply("one"), fake.Call("delete_file", `{}`), fake.Reply("two"))
	r.app.say("clean up")
	q := r.app.next(t)
	r.app.press(q.Buttons[1].ID)
	r.app.next(t) // allowed
	if got := r.app.next(t).Text; got != "one" {
		t.Fatalf("%q", got)
	}
	r.app.say("again")
	if got := r.app.next(t).Text; got != "two" {
		t.Fatalf("asked again: %q", got)
	}
}

func TestNoApprovals(t *testing.T) {
	r := rigWith(t, Config{NoApprovals: true}, true, fake.Call("delete_file", `{}`), fake.Reply("done"))
	r.app.say("clean up")
	if got := r.app.next(t).Text; got != "done" {
		t.Fatalf("asked: %q", got)
	}
}

func TestNoApprovalsStillRefusesOthers(t *testing.T) {
	b, app, p, _ := teamBridge(t, Config{Members: []string{worker}, MemberProfile: "team", NoApprovals: true})
	say(b, worker, worker, "delete it")
	app.nextIn(t, worker)
	app.nextIn(t, worker)
	b.wg.Wait()
	if p.ran.Load() {
		t.Fatal("a member's call ran")
	}
}

func TestPreview(t *testing.T) {
	if got, more := preview("a\nb"); got != "a\nb" || more != 0 {
		t.Fatalf("%q %d", got, more)
	}
	long := strings.Repeat("line\n", 40) + "end"
	got, more := preview(long)
	if strings.Count(got, "\n") != previewLines-1 || more != 41-previewLines {
		t.Fatalf("%d lines, %d more", strings.Count(got, "\n")+1, more)
	}
	if got, more := preview(strings.Repeat("x", 5000)); len(got) != previewChars || more != 1 {
		t.Fatalf("%d %d", len(got), more)
	}
}

func TestShowAll(t *testing.T) {
	content := strings.Repeat("print(1)\n", 30) + "rm -rf /"
	r := newRig(t, true, fake.Call("delete_file", `{"content":`+strconv.Quote(content)+`}`), fake.Reply("ok"))
	r.app.say("write")
	q := r.app.next(t)
	if len(q.Buttons) != 4 || strings.Contains(q.Code, "rm -rf") || !strings.Contains(q.Text, "more lines") {
		t.Fatalf("%+v", q)
	}
	r.app.press(q.Buttons[3].ID)
	if all := r.app.next(t); !strings.HasSuffix(all.Code, "  rm -rf /") {
		t.Fatalf("%q", all.Code)
	}
	r.app.press(q.Buttons[2].ID) // still open: no
	if got := r.app.next(t).Text; got != "Refused." {
		t.Fatalf("%q", got)
	}
}

func TestTheChatsLanguage(t *testing.T) {
	r := rigWith(t, Config{}, true)
	r.app.from(Message{Chat: room, From: me, Text: "/start", Language: "it-IT"})
	if got := r.app.next(t).Text; !strings.HasPrefix(got, "Ciao.") {
		t.Fatalf("%q", got)
	}
	r.app.from(Message{Chat: room, From: me, Text: "/start", Language: "de"})
	if got := r.app.next(t).Text; !strings.HasPrefix(got, "Hello.") {
		t.Fatalf("not spoken: %q", got)
	}
	r.app.from(Message{Chat: room, From: me, Text: "/start"}) // the app says nothing: the last one heard
	if got := r.app.next(t).Text; !strings.HasPrefix(got, "Hello.") {
		t.Fatalf("%q", got)
	}
}

func TestTheHostsLanguageWhenTheAppSaysNothing(t *testing.T) {
	r := rigWith(t, Config{Language: Italian}, true)
	r.app.say("/start")
	if got := r.app.next(t).Text; !strings.HasPrefix(got, "Ciao.") {
		t.Fatalf("%q", got)
	}
}

func TestHostLanguages(t *testing.T) {
	de := map[string]string{"hello": "Hallo. Schreib mir.", "too_large": "{} ist zu groß: höchstens {} MB."}
	r := rigWith(t, Config{Languages: map[string]map[string]string{"de": de}}, true)
	r.app.from(Message{Chat: room, From: me, Text: "/start", Language: "de-AT"})
	if got := r.app.next(t).Text; got != "Hallo. Schreib mir." {
		t.Fatalf("%q", got)
	}
	r.app.from(Message{Chat: room, From: me, Text: "/stop"}) // not translated: English
	if got := r.app.next(t).Text; got != "Nothing to stop." {
		t.Fatalf("%q", got)
	}
}

func TestBadLanguages(t *testing.T) {
	for name, l := range map[string]map[string]map[string]string{
		"code":         {"German": {"hello": "Hallo"}},
		"text":         {"de": {"hallo": "Hallo"}},
		"placeholders": {"de": {"too_large": "zu groß"}},
	} {
		if _, err := languages(l); err == nil {
			t.Error(name)
		}
	}
	if len(Texts()) != len(texts[English]) {
		t.Fatal("texts")
	}
}
