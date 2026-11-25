// slack_test.go: the Slack channel against a fake Slack
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agilira/go-errors"
	"github.com/coder/websocket"

	"github.com/agilira/ringil-agent-sdk/channel"
)

const (
	testBot = "xoxb-1111-2222-abcdefghijklmnop"
	testApp = "xapp-1-A111-2222-abcdefabcdef"
	botUser = "UBOT"
	team    = "T1"
)

// fake is Slack: the Web API, the socket, and the file host.
type fake struct {
	srv       *httptest.Server
	mu        sync.Mutex
	posts     []map[string]any
	reactions []string
	auths     map[string]string // method: the token it got
	acks      chan string
	toSend    chan string // envelopes the socket sends
	opens     int
	status    int // chat.postMessage's HTTP status, 0 for 200
	retries   int
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{auths: map[string]string{}, acks: make(chan string, 16), toSend: make(chan string, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/{method}", f.api)
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) { f.socket(t, w, r) })
	mux.HandleFunc("/files/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testBot {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>sign in</html>"))
			return
		}
		_, _ = w.Write([]byte("contents of " + r.PathValue("name")))
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) api(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	f.mu.Lock()
	f.auths[method] = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Unlock()
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch method {
	case "auth.test":
		writeJSON(w, map[string]any{"ok": true, "user_id": botUser, "team_id": team, "bot_id": "B1", "user": "ringil"})
	case "apps.connections.open":
		f.mu.Lock()
		f.opens++
		f.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "url": "wss" + strings.TrimPrefix(f.srv.URL, "https") + "/ws"})
	case "chat.postMessage":
		f.post(w, in)
	case "reactions.add", "reactions.remove":
		f.mu.Lock()
		f.reactions = append(f.reactions, method+" "+in["channel"].(string)+" "+in["timestamp"].(string)+" "+in["name"].(string))
		f.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	default:
		writeJSON(w, map[string]any{"ok": false, "error": "unknown_method"})
	}
}

func (f *fake) post(w http.ResponseWriter, in map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == http.StatusTooManyRequests && f.retries == 0 {
		f.retries++
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if f.status != 0 && f.status != http.StatusTooManyRequests {
		writeJSON(w, map[string]any{"ok": false, "error": "channel_not_found"})
		return
	}
	f.posts = append(f.posts, in)
	writeJSON(w, map[string]any{"ok": true, "ts": "1700000001.000200"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// socket sends a hello, then each envelope queued, reading the acks.
func (f *fake) socket(t *testing.T, w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx := r.Context()
	go f.readAcks(ctx, conn)
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","num_connections":1}`))
	for {
		select {
		case env := <-f.toSend:
			if !f.write(ctx, conn, env) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// write sends env; false ends the socket: on CLOSE, after a disconnect
// (as Slack does), on a failure.
func (f *fake) write(ctx context.Context, conn *websocket.Conn, env string) bool {
	if env == "CLOSE" {
		return false
	}
	return conn.Write(ctx, websocket.MessageText, []byte(env)) == nil && !strings.Contains(env, `"disconnect"`)
}

func (f *fake) readAcks(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var a ack
		_ = json.Unmarshal(data, &a)
		f.acks <- a.ID
	}
}

func (f *fake) channel(t *testing.T) *Channel {
	t.Helper()
	c, err := New(context.Background(), Config{BotToken: testBot, AppToken: testApp, BaseURL: f.srv.URL + "/api", Client: f.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	c.fileHost = strings.TrimPrefix(f.srv.URL, "https://")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// eventEnv is a message event in an envelope.
func eventEnv(id string, ev map[string]any) string {
	payload := map[string]any{"type": "event_callback", "team_id": team, "event": ev}
	b, _ := json.Marshal(map[string]any{"envelope_id": id, "type": "events_api", "payload": payload})
	return string(b)
}

func receive(t *testing.T, c *Channel) channel.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := c.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNewChecksTokens(t *testing.T) {
	f := newFake(t)
	for _, cfg := range []Config{
		{BotToken: "xoxp-user-token-1234567", AppToken: testApp},
		{BotToken: testBot, AppToken: "xoxb-not-an-app-token"},
		{BotToken: testBot, AppToken: testApp, BaseURL: "http://slack.example.com/api"},
	} {
		cfg.Client = f.srv.Client()
		if _, err := New(context.Background(), cfg); !errors.HasCode(err, ErrConfig) {
			t.Errorf("%+v: %v", cfg, err)
		}
	}
	c := f.channel(t)
	if c.Name() != "slack" || c.BotUser() != botUser || f.auths["auth.test"] != testBot {
		t.Fatalf("name %s user %s auth %q", c.Name(), c.BotUser(), f.auths["auth.test"])
	}
}

func TestReceiveDirectMessage(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	f.toSend <- eventEnv("e1", map[string]any{"type": "message", "channel": "D1", "channel_type": "im", "user": "U1", "text": "ciao &lt;3 &amp; tu?", "ts": "1700000000.000100"})
	m := receive(t, c)
	if m.Chat != "D1" || m.From != "U1" || m.Text != "ciao <3 & tu?" || m.Group {
		t.Fatalf("message %+v", m)
	}
	if got := <-f.acks; got != "e1" {
		t.Fatalf("ack %q", got)
	}
	if f.auths["apps.connections.open"] != testApp {
		t.Fatal("the socket was not asked with the app token")
	}
}

func TestReceiveChannelThreads(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	// Not for the bot: heard, not addressed.
	f.toSend <- eventEnv("e1", map[string]any{"type": "message", "channel": "C1", "channel_type": "channel", "user": "U1", "text": "pranzo?", "ts": "1700000000.000100"})
	// A mention starts a thread rooted at it.
	f.toSend <- eventEnv("e2", map[string]any{"type": "message", "channel": "C1", "channel_type": "channel", "user": "U1", "text": "<@UBOT> riassumi", "ts": "1700000000.000200"})
	// A reply in that thread, without a mention, is for the bot.
	f.toSend <- eventEnv("e3", map[string]any{"type": "message", "channel": "C1", "channel_type": "channel", "user": "U2", "text": "e anche questo", "ts": "1700000000.000300", "thread_ts": "1700000000.000200"})
	// Another thread, no mention: not addressed.
	f.toSend <- eventEnv("e4", map[string]any{"type": "message", "channel": "C1", "channel_type": "channel", "user": "U2", "text": "altro", "ts": "1700000000.000500", "thread_ts": "1700000000.000400"})
	want := []channel.Message{
		{Chat: "C1/1700000000.000100", From: "U1", Text: "pranzo?", Group: true},
		{Chat: "C1/1700000000.000200", From: "U1", Text: "riassumi", Group: true, Addressed: true},
		{Chat: "C1/1700000000.000200", From: "U2", Text: "e anche questo", Group: true, Addressed: true},
		{Chat: "C1/1700000000.000400", From: "U2", Text: "altro", Group: true},
	}
	for i, w := range want {
		if got := receive(t, c); got.Chat != w.Chat || got.From != w.From || got.Text != w.Text || got.Group != w.Group || got.Addressed != w.Addressed {
			t.Fatalf("%d: %+v, want %+v", i, got, w)
		}
	}
}

func TestReceiveSkipsWhatIsNoOnesWords(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	skipped := []map[string]any{
		{"type": "message", "channel": "D1", "channel_type": "im", "user": botUser, "text": "my own", "ts": "1.1"},
		{"type": "message", "channel": "D1", "channel_type": "im", "user": "U9", "bot_id": "B9", "text": "another bot", "ts": "1.2"},
		{"type": "message", "subtype": "message_changed", "channel": "D1", "channel_type": "im", "user": "U1", "ts": "1.3"},
		{"type": "message", "subtype": "channel_join", "channel": "C1", "channel_type": "channel", "user": "U1", "ts": "1.4"},
		{"type": "app_mention", "channel": "C1", "user": "U1", "text": "<@UBOT> x", "ts": "1.5"},
		{"type": "message", "channel": "c1;drop", "channel_type": "channel", "user": "U1", "text": "x", "ts": "1.6"},
	}
	for i, ev := range skipped {
		f.toSend <- eventEnv("s"+string(rune('a'+i)), ev)
	}
	// Another workspace's event, then a hello-like envelope of no type.
	other, _ := json.Marshal(map[string]any{"envelope_id": "x1", "type": "events_api", "payload": map[string]any{"type": "event_callback", "team_id": "T2",
		"event": map[string]any{"type": "message", "channel": "D1", "channel_type": "im", "user": "U1", "text": "other team", "ts": "1.7"}}})
	f.toSend <- string(other)
	f.toSend <- `not json`
	f.toSend <- eventEnv("ok", map[string]any{"type": "message", "channel": "D1", "channel_type": "im", "user": "U1", "text": "real", "ts": "1.8"})
	if m := receive(t, c); m.Text != "real" {
		t.Fatalf("got %+v", m)
	}
}

func TestReceiveButtonAndCommand(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	press, _ := json.Marshal(map[string]any{"envelope_id": "i1", "type": "interactive", "payload": map[string]any{
		"type": "block_actions", "team": map[string]any{"id": team}, "user": map[string]any{"id": "U1"},
		"channel": map[string]any{"id": "C1"}, "container": map[string]any{"thread_ts": "1700000000.000200"},
		"message": map[string]any{"ts": "1700000001.000200", "thread_ts": "1700000000.000200"},
		"actions": []any{map[string]any{"action_id": "b0", "value": "approve:abc"}}}})
	cmd, _ := json.Marshal(map[string]any{"envelope_id": "c1", "type": "slash_commands", "payload": map[string]any{
		"command": "/ringil", "text": "new", "user_id": "U1", "channel_id": "D1", "team_id": team}})
	f.toSend <- string(press)
	f.toSend <- string(cmd)
	if m := receive(t, c); m.Chat != "C1/1700000000.000200" || m.Pressed != "approve:abc" || m.From != "U1" || !m.Group {
		t.Fatalf("press %+v", m)
	}
	if m := receive(t, c); m.Chat != "D1" || m.Text != "/new" || m.Group {
		t.Fatalf("command %+v", m)
	}
}

func TestReceiveReconnectsOnDisconnect(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	f.toSend <- `{"envelope_id":"d1","type":"disconnect","payload":{"reason":"refresh_requested"}}`
	f.toSend <- eventEnv("e1", map[string]any{"type": "message", "channel": "D1", "channel_type": "im", "user": "U1", "text": "after", "ts": "1.1"})
	if m := receive(t, c); m.Text != "after" {
		t.Fatalf("got %+v", m)
	}
	f.mu.Lock()
	opens := f.opens
	f.mu.Unlock()
	if opens != 2 {
		t.Fatalf("opened %d sockets, want 2", opens)
	}
}

func TestReceiveBrokenSocketIsAnError(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	f.toSend <- "CLOSE"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Receive(ctx); !errors.HasCode(err, ErrNetwork) {
		t.Fatalf("err %v", err)
	}
	if c.conn != nil {
		t.Fatal("the broken socket was kept")
	}
}

func TestFiles(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	host := f.srv.URL
	f.toSend <- eventEnv("e1", map[string]any{"type": "message", "subtype": "file_share", "channel": "D1", "channel_type": "im", "user": "U1", "text": "guarda", "ts": "1.1",
		"files": []any{
			map[string]any{"name": "a.pdf", "mimetype": "application/pdf", "size": 10, "url_private_download": host + "/files/a.pdf"},
			map[string]any{"name": "clip.webm", "mimetype": "audio/webm", "subtype": "slack_audio", "url_private_download": host + "/files/clip.webm"},
			map[string]any{"name": "evil", "url_private_download": "https://evil.example.com/files/x"},
			map[string]any{"name": "plain", "url_private_download": strings.Replace(host, "https", "http", 1) + "/files/x"},
		}})
	m := receive(t, c)
	if len(m.Files) != 2 || m.Files[0].Name != "a.pdf" || m.Files[0].Voice || !m.Files[1].Voice {
		t.Fatalf("files %+v", m.Files)
	}
	data, err := m.Files[0].Fetch(context.Background(), 1<<20)
	if err != nil || string(data) != "contents of a.pdf" {
		t.Fatalf("fetch %q %v", data, err)
	}
	if _, err := m.Files[0].Fetch(context.Background(), 5); err == nil {
		t.Fatal("a file past the limit was read")
	}
}

func posted(f *fake) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.posts...)
}

func blocksOf(t *testing.T, p map[string]any) string {
	t.Helper()
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p["blocks"]); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestSendPlainIsLiteral(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	out := channel.Out{Text: "Approve: rm -rf <dir> **now** <!here>", Code: "echo <@U1> && ls", Buttons: []channel.Button{{ID: "y:1", Label: "Yes"}, {ID: "n:1", Label: "No"}}}
	if err := c.Send(context.Background(), "C1/1700000000.000200", out); err != nil {
		t.Fatal(err)
	}
	p := posted(f)
	if len(p) != 2 || p[0]["channel"] != "C1" || p[0]["thread_ts"] != "1700000000.000200" || p[0]["unfurl_links"] != false {
		t.Fatalf("posts %+v", p)
	}
	checkContains(t, blocksOf(t, p[0]), `"type":"plain_text"`, `rm -rf <dir> **now** <!here>`)
	checkContains(t, blocksOf(t, p[1]), "rich_text_preformatted", `"value":"y:1"`, `"value":"n:1"`)
	if text := p[0]["text"].(string); strings.Contains(text, "<") {
		t.Fatalf("notification text not escaped: %q", text)
	}
	if !c.threads.has("C1/1700000000.000200") {
		t.Fatal("a thread the bot wrote in is not followed")
	}
}

func checkContains(t *testing.T, s string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(s, w) {
			t.Fatalf("%s lacks %s", s, w)
		}
	}
}

func TestSendMarkdownCannotPing(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	if err := c.Send(context.Background(), "D1", channel.Out{Text: "**Done** <!channel> <@U1> `a<b` ok", Markdown: true}); err != nil {
		t.Fatal(err)
	}
	b := blocksOf(t, posted(f)[0])
	if !strings.Contains(b, `"type":"markdown"`) || !strings.Contains(b, `**Done** &lt;!channel> &lt;@U1> `+"`a<b`") {
		t.Fatalf("markdown %s", b)
	}
}

func TestSendSplitsLongTexts(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	long := strings.Repeat("riga di testo\n", 500) // 7000 runes
	if err := c.Send(context.Background(), "D1", channel.Out{Text: long, Buttons: []channel.Button{{ID: "x", Label: "X"}}}); err != nil {
		t.Fatal(err)
	}
	p := posted(f)
	if len(p) != 3 {
		t.Fatalf("%d posts", len(p))
	}
	for i, post := range p {
		has := strings.Contains(blocksOf(t, post), `"actions"`)
		if has != (i == len(p)-1) {
			t.Fatalf("post %d buttons %v", i, has)
		}
	}
}

func TestSendRefusesBadChatsAndReportsErrors(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	for _, chat := range []string{"", "c1", "C1/notats", "C1/1.2/3", "../x"} {
		if err := c.Send(context.Background(), chat, channel.Out{Text: "x"}); err == nil {
			t.Errorf("%q accepted", chat)
		}
	}
	if err := c.Send(context.Background(), "D1", channel.Out{}); err == nil {
		t.Error("an empty message was sent")
	}
	f.status = http.StatusNotFound
	err := c.Send(context.Background(), "D1", channel.Out{Text: "x"})
	if !errors.HasCode(err, ErrAPI) || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("err %v", err)
	}
}

func TestSendWaitsWhenAskedTo(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	f.status = http.StatusTooManyRequests
	start := time.Now()
	if err := c.Send(context.Background(), "D1", channel.Out{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 900*time.Millisecond || len(posted(f)) != 1 {
		t.Fatalf("waited %v, %d posts", time.Since(start), len(posted(f)))
	}
}

func TestRecentForgetsTheOldest(t *testing.T) {
	r := newRecent(2)
	for _, k := range []string{"a", "b", "a", "c"} {
		r.add(k)
	}
	if r.has("a") || !r.has("b") || !r.has("c") {
		t.Fatalf("%v", r.order)
	}
}

func TestCleanKeepsOnlyErrorNames(t *testing.T) {
	if clean("not_authed") != "not_authed" || clean("xoxb-123 leaked") != "error" {
		t.Fatal("clean")
	}
}

// While the agent works, the message it answers has 👀, once; the
// answer takes it away.
func TestTypingMarksTheMessage(t *testing.T) {
	f := newFake(t)
	c := f.channel(t)
	f.toSend <- eventEnv("e1", map[string]any{"type": "message", "channel": "D1", "channel_type": "im", "user": "U1", "text": "x", "ts": "1700000000.000100"})
	receive(t, c)
	for range 3 {
		if err := c.Typing(context.Background(), "D1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Send(context.Background(), "D1", channel.Out{Text: "done"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := strings.Join(f.reactions, "; ")
	f.mu.Unlock()
	if got != "reactions.add D1 1700000000.000100 eyes; reactions.remove D1 1700000000.000100 eyes" {
		t.Fatalf("reactions: %s", got)
	}
	// A chat the bot heard nothing in gets none.
	if err := c.Typing(context.Background(), "D9"); err != nil {
		t.Fatal(err)
	}
}
