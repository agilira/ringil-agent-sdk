// telegram.go: a Telegram bot as a channel
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package telegram is the Telegram channel: a bot, through the Bot API,
// receiving with long polling (no public address needed). Make the bot
// with @BotFather and give its token; the senders are the users' numeric
// IDs, which the Bridge reports when someone not allowed writes.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
)

// Error codes.
const (
	ErrConfig  errors.ErrorCode = "TELEGRAM_CONFIG"
	ErrNetwork errors.ErrorCode = "TELEGRAM_NETWORK"
	ErrAPI     errors.ErrorCode = "TELEGRAM_API"
)

// Config sets up the channel.
type Config struct {
	// Token is the bot's, from @BotFather.
	Token string
	// BaseURL is the Bot API's; default https://api.telegram.org.
	BaseURL string
	// Client makes the requests; default a new one.
	Client *http.Client
	// Poll is how long a receive waits on Telegram for messages; default
	// 30 seconds.
	Poll time.Duration
}

// Channel is a Telegram bot. Receive is for one caller at a time (the
// Bridge's); Send may be called by many.
type Channel struct {
	token, base string
	client      *http.Client
	poll        time.Duration
	offset      int64
	queue       []channel.Message
}

var validToken = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{20,100}$`)

// maxBody bounds a response of the Bot API.
const maxBody = 4 << 20

// New checks the token with Telegram and returns the channel.
func New(ctx context.Context, cfg Config) (*Channel, error) {
	if !validToken.MatchString(cfg.Token) {
		return nil, errors.New(ErrConfig, "not a Telegram bot token")
	}
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	if err := safehttp.CheckURL(base, false); err != nil {
		return nil, errors.Wrap(err, ErrConfig, "bad Bot API address")
	}
	poll := cfg.Poll
	if poll <= 0 {
		poll = 30 * time.Second
	}
	c := &Channel{token: cfg.Token, base: base, poll: poll,
		client: safehttp.Client(cfg.Client, nil, maxBody, false)}
	if err := c.call(ctx, "getMe", struct{}{}, nil); err != nil {
		return nil, err
	}
	return c, nil
}

// Name is "telegram".
func (c *Channel) Name() string { return "telegram" }

// Receive returns the next message or button pressed.
func (c *Channel) Receive(ctx context.Context) (channel.Message, error) {
	for len(c.queue) == 0 {
		if err := c.fetch(ctx); err != nil {
			return channel.Message{}, err
		}
	}
	m := c.queue[0]
	c.queue = c.queue[1:]
	return m, nil
}

type updatesIn struct {
	Offset  int64    `json:"offset"`
	Timeout int      `json:"timeout"`
	Allowed []string `json:"allowed_updates"`
}

// fetch waits for updates and queues their messages.
func (c *Channel) fetch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.poll+15*time.Second)
	defer cancel()
	var updates []update
	in := updatesIn{Offset: c.offset, Timeout: int(c.poll / time.Second), Allowed: []string{"message", "callback_query"}}
	if err := c.call(ctx, "getUpdates", in, &updates); err != nil {
		return err
	}
	for _, u := range updates {
		c.offset = max(c.offset, u.ID+1)
		if m, ok := c.message(ctx, u); ok {
			c.queue = append(c.queue, m)
		}
	}
	return nil
}

type update struct {
	ID       int64     `json:"update_id"`
	Message  *message  `json:"message"`
	Callback *callback `json:"callback_query"`
}

type message struct {
	Chat    struct{ ID int64 } `json:"chat"`
	From    *user              `json:"from"`
	Text    string             `json:"text"`
	Caption string             `json:"caption"`
}

type user struct {
	ID int64 `json:"id"`
}

type callback struct {
	ID      string   `json:"id"`
	From    user     `json:"from"`
	Message *message `json:"message"`
	Data    string   `json:"data"`
}

// message turns an update into a channel message; ok is false for what
// is neither a message with a sender nor a button of one of ours.
func (c *Channel) message(ctx context.Context, u update) (channel.Message, bool) {
	switch {
	case u.Message != nil && u.Message.From != nil:
		m := u.Message
		return channel.Message{Chat: id(m.Chat.ID), From: id(m.From.ID), Text: m.Text + m.Caption}, true
	case u.Callback != nil && u.Callback.Message != nil:
		cb := u.Callback
		// Stops the button's spinner; only cosmetic, so not retried.
		_ = c.call(ctx, "answerCallbackQuery", map[string]string{"callback_query_id": cb.ID}, nil)
		return channel.Message{Chat: id(cb.Message.Chat.ID), From: id(cb.From.ID), Pressed: cb.Data}, true
	}
	return channel.Message{}, false
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

type sendIn struct {
	Chat   string  `json:"chat_id"`
	Text   string  `json:"text"`
	Markup *markup `json:"reply_markup,omitempty"`
}

type markup struct {
	Keyboard [][]button `json:"inline_keyboard"`
}

type button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

// Send delivers out, in parts when longer than Telegram takes; the
// buttons go under the last part.
func (c *Channel) Send(ctx context.Context, chat string, out channel.Out) error {
	parts := split(out.Text, maxText)
	if len(parts) == 0 {
		return errors.New(ErrAPI, "nothing to send")
	}
	for i, p := range parts {
		in := sendIn{Chat: chat, Text: p}
		if i == len(parts)-1 && len(out.Buttons) > 0 {
			in.Markup = keyboard(out.Buttons)
		}
		if err := c.call(ctx, "sendMessage", in, nil); err != nil {
			return err
		}
	}
	return nil
}

func keyboard(buttons []channel.Button) *markup {
	row := make([]button, 0, len(buttons))
	for _, b := range buttons {
		row = append(row, button{Text: b.Label, Data: b.ID})
	}
	return &markup{Keyboard: [][]button{row}}
}

type reply struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// maxRetryWait bounds how long a call waits when Telegram asks to slow
// down; longer, it fails.
const maxRetryWait = time.Minute

// call runs a Bot API method, once more when Telegram asks to wait.
func (c *Channel) call(ctx context.Context, method string, in, out any) error {
	r, err := c.post(ctx, method, in)
	if err == nil && !r.OK && wait(r) > 0 {
		pauseFor(ctx, wait(r))
		r, err = c.post(ctx, method, in)
	}
	if err != nil {
		return err
	}
	if !r.OK {
		return errors.New(ErrAPI, "telegram "+method+": "+c.scrub(r.Description))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return errors.Wrap(err, ErrAPI, "telegram "+method+": unreadable result")
	}
	return nil
}

// wait is how long Telegram asks to wait, when it is not too long.
func wait(r reply) time.Duration {
	d := time.Duration(r.Parameters.RetryAfter) * time.Second
	if d > maxRetryWait {
		return 0
	}
	return d
}

func pauseFor(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func (c *Channel) post(ctx context.Context, method string, in any) (reply, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return reply{}, errors.Wrap(err, ErrAPI, "telegram "+method+": cannot encode the request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return reply{}, errors.New(ErrNetwork, "telegram "+method+": bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return reply{}, errors.New(ErrNetwork, "telegram "+method+": "+c.scrub(netError(err)))
	}
	defer func() { _ = resp.Body.Close() }()
	var r reply
	data, err := io.ReadAll(resp.Body)
	if err == nil {
		err = json.Unmarshal(data, &r)
	}
	if err != nil {
		return reply{}, errors.New(ErrAPI, "telegram "+method+": unreadable reply (HTTP "+strconv.Itoa(resp.StatusCode)+")")
	}
	return r, nil
}

// netError is err without the request's URL, which holds the token.
func netError(err error) string {
	var u *url.Error
	if stderrors.As(err, &u) {
		return u.Err.Error()
	}
	return err.Error()
}

// scrub removes the token from a text, wherever it came from.
func (c *Channel) scrub(s string) string {
	return strings.ReplaceAll(s, c.token, "<token>")
}
