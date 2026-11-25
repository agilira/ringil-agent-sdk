// slack.go: a Slack app as a channel
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package slack is the Slack channel: an app in Socket Mode, so no
// public address is needed. It takes the bot token (xoxb-) for the Web
// API and the app-level token (xapp-, scope connections:write) for the
// socket; docs/slack/manifest.json creates the app with what it needs.
//
// Chats are a direct message (its channel ID), or a thread of a channel
// ("C123/1700000000.000100"): a message in a channel starts a thread,
// and the answers go into it, as people use Slack. In a channel the
// Bridge answers a message that mentions the bot, or any message of a
// thread the bot is in. Senders are Slack member IDs (U…). The app's
// slash command (/ringil new) comes as the Bridge's command (/new).
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"
	"github.com/coder/websocket"

	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
)

// Error codes.
const (
	ErrConfig  errors.ErrorCode = "SLACK_CONFIG"
	ErrNetwork errors.ErrorCode = "SLACK_NETWORK"
	ErrAPI     errors.ErrorCode = "SLACK_API"
)

// Config sets up the channel.
type Config struct {
	// BotToken is the bot's (xoxb-), from Install App.
	BotToken string
	// AppToken is the app-level token (xapp-) with connections:write,
	// from Basic Information.
	AppToken string
	// BaseURL is the Web API's; default https://slack.com/api.
	BaseURL string
	// Client makes the requests; default a new one.
	Client *http.Client
}

// Channel is a Slack app. Receive is for one caller at a time (the
// Bridge's); Send may be called by many.
type Channel struct {
	bot, app, base string
	client         *http.Client
	fetcher        *http.Client
	dialer         *http.Client // the socket's: the host's client as it is
	fileHost       string       // where shared files are; files.slack.com
	self           identity
	mention        *regexp.Regexp

	conn    *websocket.Conn // the socket, nil until Receive opens it
	threads *recent         // threads the bot is in: their messages are for it
	marks   *marks          // the messages being answered, for 👀
}

// identity is the bot, as auth.test gives it.
type identity struct {
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
	TeamID string `json:"team_id"`
	User   string `json:"user"`
}

var (
	botToken = regexp.MustCompile(`^xoxb-[A-Za-z0-9-]{10,250}$`)
	appToken = regexp.MustCompile(`^xapp-[A-Za-z0-9-]{10,250}$`)
)

// maxBody bounds a response of the Web API.
const maxBody = 4 << 20

// New checks the tokens with Slack and returns the channel.
func New(ctx context.Context, cfg Config) (*Channel, error) {
	if !botToken.MatchString(cfg.BotToken) || !appToken.MatchString(cfg.AppToken) {
		return nil, errors.New(ErrConfig, "Slack needs a bot token (xoxb-…) and an app-level token (xapp-…)")
	}
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if base == "" {
		base = "https://slack.com/api"
	}
	if err := safehttp.CheckURL(base, false); err != nil {
		return nil, errors.Wrap(err, ErrConfig, "bad Slack API address")
	}
	c := &Channel{bot: cfg.BotToken, app: cfg.AppToken, base: base,
		client:  safehttp.Client(cfg.Client, nil, maxBody, false),
		fetcher: safehttp.Client(cfg.Client, nil, 0, false),
		dialer:  cfg.Client, fileHost: "files.slack.com",
		threads: newRecent(maxThreads), marks: newMarks()}
	if err := c.call(ctx, c.bot, "auth.test", struct{}{}, &c.self); err != nil {
		return nil, err
	}
	if c.self.UserID == "" {
		return nil, errors.New(ErrAPI, "slack auth.test: no bot user")
	}
	c.mention = regexp.MustCompile(`<@` + regexp.QuoteMeta(c.self.UserID) + `(\|[^>]*)?>`)
	return c, nil
}

// Name is "slack".
func (c *Channel) Name() string { return "slack" }

// BotUser is the bot's member ID, from auth.test.
func (c *Channel) BotUser() string { return c.self.UserID }

// drop leaves a socket Slack ended or that broke, without waiting on it.
func (c *Channel) drop() {
	if c.conn != nil {
		_ = c.conn.CloseNow()
		c.conn = nil
	}
}

// Close closes the socket.
func (c *Channel) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close(websocket.StatusNormalClosure, "")
	c.conn = nil
	return err
}

// reply is the envelope of every Web API answer.
type reply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// maxRetryWait bounds how long a call waits when Slack asks to slow
// down; longer, it fails.
const maxRetryWait = time.Minute

// call runs a Web API method with token, once more when Slack asks to
// wait, and decodes the answer into out.
func (c *Channel) call(ctx context.Context, token, method string, in, out any) error {
	data, wait, err := c.post(ctx, token, method, in)
	if err == nil && wait > 0 {
		pauseFor(ctx, wait)
		data, _, err = c.post(ctx, token, method, in)
	}
	if err != nil {
		return err
	}
	var r reply
	if err := json.Unmarshal(data, &r); err != nil {
		return errors.Wrap(err, ErrAPI, "slack "+method+": unreadable answer")
	}
	if !r.OK {
		return errors.New(ErrAPI, "slack "+method+": "+clean(r.Error))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.Wrap(err, ErrAPI, "slack "+method+": unreadable answer")
	}
	return nil
}

// post sends one request; wait is set when Slack answered 429 with a
// Retry-After short enough to honour.
func (c *Channel) post(ctx context.Context, token, method string, in any) ([]byte, time.Duration, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, 0, errors.Wrap(err, ErrAPI, "slack "+method+": cannot encode the request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errors.Wrap(err, ErrAPI, "slack "+method+": cannot build the request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, errors.Wrap(err, ErrNetwork, "slack "+method+": cannot reach Slack")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, retryAfter(resp.Header.Get("Retry-After")), nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, errors.New(ErrAPI, "slack "+method+": HTTP "+strconv.Itoa(resp.StatusCode))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, errors.Wrap(err, ErrNetwork, "slack "+method+": cannot read the answer")
	}
	return data, 0, nil
}

// retryAfter is Slack's wait in seconds, when it is not too long; a
// missing or bad header waits one second.
func retryAfter(h string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	switch {
	case err != nil || n <= 0:
		return time.Second
	case time.Duration(n)*time.Second > maxRetryWait:
		return 0
	}
	return time.Duration(n) * time.Second
}

func pauseFor(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

var errorName = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// clean keeps Slack's error name when it is one ("channel_not_found");
// anything else is not repeated.
func clean(s string) string {
	if errorName.MatchString(s) {
		return s
	}
	return "error"
}

// recent remembers the last n keys: the threads the bot is in.
type recent struct {
	mu    sync.Mutex
	max   int
	keys  map[string]bool
	order []string
}

// maxThreads is how many threads the bot follows without a mention.
const maxThreads = 10000

func newRecent(n int) *recent { return &recent{max: n, keys: map[string]bool{}} }

func (r *recent) add(k string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keys[k] {
		return
	}
	r.keys[k] = true
	r.order = append(r.order, k)
	if len(r.order) > r.max {
		delete(r.keys, r.order[0])
		r.order = r.order[1:]
	}
}

func (r *recent) has(k string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.keys[k]
}
