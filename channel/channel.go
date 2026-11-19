// channel.go: what a chat app must do to talk with the agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package channel connects chat apps (Telegram, Slack, anything else) to
// the agents of a serve.Server. An adapter for an app implements Channel:
// receive a message, send one. Everything else is the Bridge's, the same
// for every app: each chat is a conversation of the server, only the
// senders the host allows are heard, and the tool calls the server wants
// approved are asked in the chat, with yes/no buttons where the app has
// them and a written yes or no where it has not.
//
//	tg, _ := telegram.New(ctx, telegram.Config{Token: token})
//	b, _ := channel.NewBridge(channel.Config{Channel: tg, Server: srv, Allow: []string{"123456789"}})
//	b.Run(ctx)
//
// Writing an adapter: Receive blocks until a message comes and returns
// errors as they are (the Bridge retries, waiting longer each time); it
// is called by one goroutine at a time. Send delivers a text to a chat,
// splitting it if the app has a limit, and shows the buttons if the app
// can, else ignores them; it is called by several goroutines at once
// (one per chat), never twice at once for the same chat. Chat and From
// are the app's own identifiers, as strings.
package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Channel is a chat app, as the Bridge sees it.
type Channel interface {
	// Name names the app: 1-20 lower-case letters, digits or -. It
	// prefixes the server's conversations, so it must stay the same.
	Name() string
	// Receive blocks until a message comes, or ctx ends; one caller at a
	// time.
	Receive(ctx context.Context) (Message, error)
	// Send delivers out to a chat. It is called for several chats at
	// once, in order for each chat.
	Send(ctx context.Context, chat string, out Out) error
}

// Message is what someone sent in a chat: a text, or a button pressed.
type Message struct {
	// Chat is the conversation: the replies go there.
	Chat string
	// From is who sent it: only the senders the host allows are heard.
	From string
	Text string
	// Pressed is the ID of the button pressed, when the message is that.
	Pressed string
}

// Out is what the Bridge sends.
type Out struct {
	Text string
	// Buttons, when the app has them, go under the text; an app without
	// them ignores them (the text says what to write instead).
	Buttons []Button
}

// Button is a choice under a message; pressing it comes back as a
// Message with Pressed set to its ID.
type Button struct {
	ID    string
	Label string
}

var validName = regexp.MustCompile(`^[a-z0-9-]{1,20}$`)

var plainChat = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// Conversation is the server's conversation for a chat of a channel:
// "<name>-<chat>", or "<name>-h<digest>" when the chat's identifier is
// not made of letters, digits, _ and - only, or is long.
func Conversation(name, chat string) string {
	if plainChat.MatchString(chat) {
		return name + "-" + chat
	}
	sum := sha256.Sum256([]byte(chat))
	return name + "-h" + hex.EncodeToString(sum[:16])
}

// answer reads a written yes or no; ok is false for anything else.
func answer(text string) (yes, ok bool) {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(text), ".!")) {
	case "yes", "y", "si", "sì", "s":
		return true, true
	case "no", "n":
		return false, true
	}
	return false, false
}

// command reads a chat command, also in the form "/stop@BotName" that
// group chats use.
func command(text, name string) bool {
	word, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(text)), "@")
	return word == name
}

// visible is s without the characters that hide or reorder text on
// screen (controls, bidi marks, zero-width and other format characters),
// so that what a user approves is what they read. Line breaks and tabs
// stay.
func visible(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == 0x2028, r == 0x2029:
			return -1
		}
		return r
	}, s)
}

// Bounds of an input shown in an approval: past maxShown characters,
// its start and its end, and how much is not shown.
const (
	maxShown  = 12000
	shownHead = 9000
	shownTail = 2000
)

// showInput is a tool call's input as the user reads it to approve it:
// indented JSON, made visible; a very long one is cut in the middle,
// saying so.
func showInput(raw json.RawMessage) string {
	var v any
	s := string(raw)
	if json.Unmarshal(raw, &v) == nil {
		if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
			s = string(pretty)
		}
	}
	r := []rune(visible(s))
	if len(r) <= maxShown {
		return string(r)
	}
	hidden := len(r) - shownHead - shownTail
	return string(r[:shownHead]) + "\n[… " + strconv.Itoa(hidden) + " characters not shown: refuse if you need to see them …]\n" + string(r[len(r)-shownTail:])
}
