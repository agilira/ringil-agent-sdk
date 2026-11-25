// socket.go: receiving through Socket Mode
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package slack

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/agilira/go-errors"
	"github.com/coder/websocket"

	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
)

// maxFrame bounds one message of the socket; Slack's are a few KiB.
const maxFrame = 1 << 20

// envelope is what Socket Mode sends: an event, an interaction, a slash
// command, or word about the connection.
type envelope struct {
	ID      string          `json:"envelope_id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type ack struct {
	ID string `json:"envelope_id"`
}

// Receive returns the next message, button pressed or command. It opens
// the socket when needed and opens a new one when Slack asks (a refresh,
// every few hours); a broken socket is an error, and the next call
// opens a new one.
func (c *Channel) Receive(ctx context.Context) (channel.Message, error) {
	for {
		if c.conn == nil {
			if err := c.open(ctx); err != nil {
				return channel.Message{}, err
			}
		}
		env, err := c.next(ctx)
		if err != nil {
			c.drop()
			return channel.Message{}, err
		}
		if m, ok := c.handle(env); ok {
			return m, nil
		}
	}
}

type connectionsOpen struct {
	URL string `json:"url"`
}

// open asks Slack for a socket address with the app token and dials it.
func (c *Channel) open(ctx context.Context) error {
	var r connectionsOpen
	if err := c.call(ctx, c.app, "apps.connections.open", struct{}{}, &r); err != nil {
		return err
	}
	if err := checkSocketURL(r.URL); err != nil {
		return err
	}
	conn, resp, err := websocket.Dial(ctx, r.URL, &websocket.DialOptions{HTTPClient: c.dialer})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return errors.Wrap(err, ErrNetwork, "slack: cannot open the socket")
	}
	conn.SetReadLimit(maxFrame)
	c.conn = conn
	return nil
}

// checkSocketURL: wss, or ws to loopback (a test).
func checkSocketURL(raw string) error {
	u, err := url.Parse(raw)
	if err == nil && u.User == nil && (u.Scheme == "wss" || (u.Scheme == "ws" && safehttp.Loopback(u.Hostname()))) {
		return nil
	}
	return errors.New(ErrAPI, "slack gave a socket address that is not wss")
}

// next reads one envelope and acknowledges it at once, as Slack wants
// within three seconds; what it holds is handled after.
func (c *Channel) next(ctx context.Context) (envelope, error) {
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		return envelope{}, errors.Wrap(err, ErrNetwork, "slack: the socket closed")
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return envelope{}, nil // not ours to read: skipped
	}
	if env.ID != "" {
		out, _ := json.Marshal(ack{ID: env.ID})
		if err := c.conn.Write(ctx, websocket.MessageText, out); err != nil {
			return envelope{}, errors.Wrap(err, ErrNetwork, "slack: cannot acknowledge")
		}
	}
	return env, nil
}

// handle turns an envelope into a message; ok is false for what is
// none (a hello, an event of no one, a bot's own message). A disconnect
// closes the socket, so the next read opens a new one.
func (c *Channel) handle(env envelope) (channel.Message, bool) {
	switch env.Type {
	case "events_api":
		return c.event(env.Payload)
	case "interactive":
		return c.interaction(env.Payload)
	case "slash_commands":
		return c.command(env.Payload)
	case "disconnect":
		c.drop()
	}
	return channel.Message{}, false
}
