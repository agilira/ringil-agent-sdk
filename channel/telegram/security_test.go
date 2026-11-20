// security_test.go: fuzz and XSS/spoofing/traversal/token-leak security tests for Telegram
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
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzFormat(f *testing.F) {
	seeds := []string{
		"hello **world** and *italics* and ~~strike~~",
		"# Title\n- item 1\n- item 2",
		"`inline <code>` and [link](https://example.com)",
		"<script>alert(1)</script>",
		"<img src=x onerror=alert(1)>",
		"[click](javascript:alert(1))",
		`[click](https://example.com"onclick="evil)`,
		"```\n</pre><script>alert(1)</script>\n```",
		"`</code><script>x</script>`",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, md string) {
		if len(md) > 2048 || !utf8.ValidString(md) {
			return
		}
		out := format(md)
		if !utf8.ValidString(out) {
			t.Fatalf("format produced invalid UTF-8")
		}
		lower := strings.ToLower(out)
		for _, bad := range []string{"<script", "<img", "<iframe", "<svg", "<object", "<style", "<form", `href="javascript:`} {
			if strings.Contains(lower, bad) {
				t.Fatalf("unsafe HTML %q in format(%q) -> %q", bad, md, out)
			}
		}
	})
}

type roundTripOK struct{}

func (roundTripOK) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
		Header:     make(http.Header),
	}, nil
}

func FuzzUpdateParse(f *testing.F) {
	seeds := []string{
		`{"update_id":1,"message":{"chat":{"id":7,"type":"private"},"from":{"id":5,"language_code":"it"},"text":"hello"}}`,
		`{"update_id":2,"message":{"chat":{"id":-5,"type":"supergroup"},"from":{"id":5},"sender_chat":{"id":-5},"text":"@ringilbot hi"}}`,
		`{"update_id":3,"message":{"chat":{"id":-5,"type":"group"},"from":{"id":1087968824},"text":"anon"}}`,
		`{"update_id":4,"message":{"chat":{"id":7,"type":"private"},"from":{"id":5},"contact":{"phone_number":"+3912345678","user_id":999}}}`,
		`{"update_id":5,"message":{"chat":{"id":7,"type":"private"},"from":{"id":5},"contact":{"phone_number":"+3912345678","user_id":5}}}`,
		`{"update_id":6,"callback_query":{"id":"cb1","from":{"id":5},"message":{"chat":{"id":7,"type":"private"}},"data":"tok:y"}}`,
		`{"update_id":7,"message":{"chat":{"id":7,"type":"private"},"from":{"id":5},"photo":[{"file_id":"a","file_size":10}]}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	ch := &Channel{
		token:  "123:abcdefghijklmnopqrstuv",
		base:   "https://api.telegram.org",
		client: &http.Client{Transport: roundTripOK{}},
		bot:    me{ID: 999, Username: "ringilbot"},
		name:   regexp.MustCompile(`(?i)@ringilbot\b`),
		unkey:  map[string]bool{},
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 8192 {
			return
		}
		var u update
		if json.Unmarshal(raw, &u) != nil {
			return
		}
		q, ok := ch.message(context.Background(), u)
		if ok {
			checkParsedUpdate(t, u, q)
		}
	})
}

func checkParsedUpdate(t *testing.T, u update, q queued) {
	t.Helper()
	if u.Message == nil || u.Message.From == nil {
		return
	}
	if anonSender(u.Message) && q.From != "" {
		t.Fatalf("shared/sender_chat message assigned sender %q", q.From)
	}
	if q.Phone != "" && !validOwnPrivateContact(u.Message) {
		t.Fatalf("untrusted contact phone %q accepted for %+v", q.Phone, u.Message)
	}
}

func anonSender(m *message) bool { return m.SenderChat != nil || shared[m.From.ID] }

func validOwnPrivateContact(m *message) bool {
	return m.Chat.Type == "private" && m.Contact != nil && m.Contact.UserID != 0 && m.Contact.UserID == m.From.ID
}

func TestSecurityContactSpoofingAndAnonymousSenders(t *testing.T) {
	u := &user{ID: 42}
	badContacts := []*message{
		{Chat: struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		}{ID: -5, Type: "group"}, From: u, Contact: &contact{Phone: "+3912345678", UserID: 42}},
		{Chat: struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		}{ID: 42, Type: "private"}, From: u, Contact: &contact{Phone: "+3912345678", UserID: 0}},
		{Chat: struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		}{ID: 42, Type: "private"}, From: u, Contact: &contact{Phone: "+3912345678", UserID: 99}},
	}
	for i, m := range badContacts {
		if got := phone(m); got != "" {
			t.Errorf("case %d: spoofed phone %q accepted", i, got)
		}
	}
	okMsg := &message{Chat: struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	}{ID: 42, Type: "private"}, From: u, Contact: &contact{Phone: "+3912345678", UserID: 42}}
	if got := phone(okMsg); got != "+3912345678" {
		t.Fatalf("valid own contact rejected: %q", got)
	}
	for _, id := range []int64{1087968824, 136817688, 777000} {
		if got := sender(&message{From: &user{ID: id}}); got != "" {
			t.Errorf("shared Telegram ID %d returned %q", id, got)
		}
	}
	if got := sender(&message{From: u, SenderChat: &struct{}{}}); got != "" {
		t.Errorf("SenderChat message returned %q", got)
	}
}

func TestSecurityTokenNeverLeaksInErrors(t *testing.T) {
	b, srv := newBotAPI(t)
	ch := open(t, srv.URL)
	ctx := context.Background()

	b.set("sendMessage", `{"ok":false,"description":"failed at /bot`+botToken+`/sendMessage"}`)
	if err := ch.call(ctx, "sendMessage", map[string]string{"chat_id": "1", "text": "x"}, nil); err == nil || strings.Contains(err.Error(), botToken) {
		t.Fatalf("API error leaked token or succeeded: %v", err)
	}

	b.set("getFile", `{"ok":true,"result":{"file_path":"missing/x.bin","file_size":4}}`)
	if _, err := ch.download(ctx, "f1", 100); err == nil || strings.Contains(err.Error(), botToken) {
		t.Fatalf("download error leaked token or succeeded: %v", err)
	}

	srv.Close()
	if err := ch.call(ctx, "getMe", struct{}{}, nil); err == nil || strings.Contains(err.Error(), botToken) {
		t.Fatalf("network error leaked token or succeeded: %v", err)
	}
}

func TestSecurityFileDownloadPathTraversalAndOversize(t *testing.T) {
	b, _ := newBotAPI(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/file/bot"+botToken+"/documents/big.bin", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("A", 200)))
	})
	mux.Handle("/", http.HandlerFunc(b.serve))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ch := open(t, srv.URL)
	ctx := context.Background()

	for _, p := range []string{"../etc/passwd", "/etc/passwd", "a/../b", "a/..", `a\b`, "a?x=1", "a#frag", ""} {
		b.set("getFile", `{"ok":true,"result":{"file_path":"`+strings.ReplaceAll(p, `\`, `\\`)+`","file_size":5}}`)
		if _, err := ch.download(ctx, "id", 100); err == nil {
			t.Errorf("traversal path %q accepted", p)
		}
	}
	b.set("getFile", `{"ok":true,"result":{"file_path":"documents/big.bin","file_size":200}}`)
	if _, err := ch.download(ctx, "id", 50); err == nil {
		t.Fatal("oversize file_size accepted")
	}
	b.set("getFile", `{"ok":true,"result":{"file_path":"documents/big.bin","file_size":10}}`)
	if _, err := ch.download(ctx, "id", 50); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("lying file_size not caught on stream: %v", err)
	}
}

func TestSecurityFormatNeutralizesXSS(t *testing.T) {
	cases := []struct {
		in, mustNotContain string
	}{
		{"<script>alert(1)</script>", "<script>"},
		{"<img src=x onerror=alert(1)>", "<img"},
		{"[click](javascript:alert(1))", "href=\"javascript:"},
		{"[click](data:text/html,hi)", "href=\"data:"},
		{`[click](https://a.com"onclick="evil)`, `"onclick="`},
		{"```\n</pre><script>x</script>\n```", "<script>"},
		{"`</code><script>x</script>`", "<script>"},
	}
	for _, c := range cases {
		out := format(c.in)
		if strings.Contains(strings.ToLower(out), strings.ToLower(c.mustNotContain)) {
			t.Errorf("format(%q) leaked %q in %q", c.in, c.mustNotContain, out)
		}
	}
}
