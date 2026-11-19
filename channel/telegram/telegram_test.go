// telegram_test.go: the channel against a fake Bot API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/agilira/ringil-agent-sdk/channel"
)

const botToken = "123456:ABCdefGHIjklMNOpqrSTUvwxYZ0123456789"

// botAPI is a fake Bot API: canned results by method, calls recorded.
type botAPI struct {
	mu      sync.Mutex
	replies map[string][]string // method -> bodies, the last one repeats
	calls   []call
}

type call struct {
	method string
	body   map[string]any
}

func newBotAPI(t *testing.T) (*botAPI, *httptest.Server) {
	t.Helper()
	b := &botAPI{replies: map[string][]string{
		"getMe":               {`{"ok":true,"result":{"id":1}}`},
		"sendMessage":         {`{"ok":true,"result":{}}`},
		"answerCallbackQuery": {`{"ok":true,"result":true}`},
		"getUpdates":          {`{"ok":true,"result":[]}`},
	}}
	srv := httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(srv.Close)
	return b, srv
}

func (b *botAPI) serve(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+botToken+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(data, &body)
	b.mu.Lock()
	b.calls = append(b.calls, call{method, body})
	list := b.replies[method]
	out := `{"ok":false,"description":"no such method"}`
	if len(list) > 0 {
		out = list[0]
		if len(list) > 1 {
			b.replies[method] = list[1:]
		}
	}
	b.mu.Unlock()
	_, _ = io.WriteString(w, out)
}

func (b *botAPI) set(method string, bodies ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.replies[method] = bodies
}

func (b *botAPI) called(method string) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, c := range b.calls {
		if c.method == method {
			out = append(out, c.body)
		}
	}
	return out
}

func open(t *testing.T, url string) *Channel {
	t.Helper()
	c, err := New(context.Background(), Config{Token: botToken, BaseURL: url, Poll: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewChecks(t *testing.T) {
	b, srv := newBotAPI(t)
	for _, cfg := range []Config{
		{Token: "nope", BaseURL: srv.URL},
		{Token: botToken + "/../x", BaseURL: srv.URL},
		{Token: botToken, BaseURL: "http://example.com"},
	} {
		if _, err := New(context.Background(), cfg); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
	b.set("getMe", `{"ok":false,"description":"Unauthorized"}`)
	_, err := New(context.Background(), Config{Token: botToken, BaseURL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("bad token accepted: %v", err)
	}
}

func TestTheTokenNeverShows(t *testing.T) {
	_, srv := newBotAPI(t)
	c := open(t, srv.URL)
	srv.Close()
	err := c.Send(context.Background(), "1", channel.Out{Text: "hi"})
	if err == nil || strings.Contains(err.Error(), botToken) || strings.Contains(err.Error(), "ABCdef") {
		t.Fatalf("error %v", err)
	}
	c.token = "1:" + strings.Repeat("x", 30)
	if got := c.scrub("bad " + c.token + " here"); strings.Contains(got, "xxxx") {
		t.Fatalf("scrub %q", got)
	}
}

func TestReceive(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	b.set("getUpdates", `{"ok":true,"result":[
		{"update_id":10,"message":{"chat":{"id":-100},"from":{"id":5},"text":"hello"}},
		{"update_id":11,"message":{"chat":{"id":-100},"text":"no sender"}},
		{"update_id":12,"message":{"chat":{"id":7},"from":{"id":5},"caption":"a photo"}},
		{"update_id":13,"callback_query":{"id":"cb1","from":{"id":5},"message":{"chat":{"id":7}},"data":"tok:y"}},
		{"update_id":14,"callback_query":{"id":"cb2","from":{"id":5},"data":"inline"}}]}`,
		`{"ok":true,"result":[]}`)
	want := []channel.Message{
		{Chat: "-100", From: "5", Text: "hello"},
		{Chat: "7", From: "5", Text: "a photo"},
		{Chat: "7", From: "5", Pressed: "tok:y"},
	}
	for i, w := range want {
		m, err := c.Receive(context.Background())
		if err != nil || m != w {
			t.Fatalf("message %d: %+v %v", i, m, err)
		}
	}
	if a := b.called("answerCallbackQuery"); len(a) != 1 || a[0]["callback_query_id"] != "cb1" {
		t.Fatalf("callbacks answered: %v", a)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _ = c.Receive(ctx) // asks again, from after the last update
	ups := b.called("getUpdates")
	if last := ups[len(ups)-1]; last["offset"] != float64(15) {
		t.Fatalf("offset %v", last["offset"])
	}
}

func TestReceiveError(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	b.set("getUpdates", `{"ok":false,"description":"Conflict: another getUpdates"}`)
	if _, err := c.Receive(context.Background()); err == nil || !strings.Contains(err.Error(), "Conflict") {
		t.Fatalf("error %v", err)
	}
}

func TestSendWithButtons(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	err := c.Send(context.Background(), "-100", channel.Out{Text: "allow?", Buttons: []channel.Button{{ID: "t:y", Label: "Yes"}, {ID: "t:n", Label: "No"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := b.called("sendMessage")
	if len(s) != 1 || s[0]["chat_id"] != "-100" || s[0]["text"] != "allow?" {
		t.Fatalf("sent %v", s)
	}
	kb, _ := json.Marshal(s[0]["reply_markup"])
	if string(kb) != `{"inline_keyboard":[[{"callback_data":"t:y","text":"Yes"},{"callback_data":"t:n","text":"No"}]]}` {
		t.Fatalf("keyboard %s", kb)
	}
}

func TestSendLongTextInParts(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	text := strings.Repeat("line of text\n", 1000)
	if err := c.Send(context.Background(), "1", channel.Out{Text: text, Buttons: []channel.Button{{ID: "x", Label: "X"}}}); err != nil {
		t.Fatal(err)
	}
	s := b.called("sendMessage")
	if len(s) < 4 {
		t.Fatalf("%d parts", len(s))
	}
	for i, m := range s {
		_, has := m["reply_markup"]
		if has != (i == len(s)-1) {
			t.Fatalf("part %d buttons %v", i, has)
		}
	}
}

func TestSlowDownIsHonoured(t *testing.T) {
	b, srv := newBotAPI(t)
	c := open(t, srv.URL)
	b.set("sendMessage", `{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":1}}`, `{"ok":true,"result":{}}`)
	if err := c.Send(context.Background(), "1", channel.Out{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if n := len(b.called("sendMessage")); n != 2 {
		t.Fatalf("%d sends", n)
	}
	b.set("sendMessage", `{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":3600}}`)
	if err := c.Send(context.Background(), "1", channel.Out{Text: "hi"}); err == nil {
		t.Fatal("an hour's wait was waited")
	}
}

func TestNothingToSend(t *testing.T) {
	_, srv := newBotAPI(t)
	if err := open(t, srv.URL).Send(context.Background(), "1", channel.Out{Text: " \n"}); err == nil {
		t.Fatal("blank text sent")
	}
}

func TestSplit(t *testing.T) {
	if got := split("  \n ", 10); len(got) != 0 {
		t.Fatalf("blank: %q", got)
	}
	if got := split("abc\ndefgh", 6); len(got) != 2 || got[0] != "abc" || got[1] != "defgh" {
		t.Fatalf("at the line: %q", got)
	}
	if got := split("abcdefgh", 3); strings.Join(got, "") != "abcdefgh" || len(got) != 3 {
		t.Fatalf("no line: %q", got)
	}
	if got := split("😀😀😀", 4); len(got) != 2 || got[0] != "😀😀" {
		t.Fatalf("emoji count two units: %q", got)
	}
}

func FuzzSplit(f *testing.F) {
	f.Add("hello\nworld", 5)
	f.Add("😀a\n\nb", 2)
	f.Fuzz(func(t *testing.T, text string, limit int) {
		if limit < 2 || limit > 5000 {
			return
		}
		for _, p := range split(text, limit) {
			if n := len(utf16.Encode([]rune(p))); n > limit {
				t.Fatalf("part of %d units, limit %d", n, limit)
			}
			if strings.TrimSpace(p) == "" {
				t.Fatal("blank part")
			}
		}
	})
}
