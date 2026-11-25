// events.go: Slack's events, buttons and commands as channel messages
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package slack

import (
	"encoding/json"
	"strings"

	"github.com/agilira/ringil-agent-sdk/channel"
)

type eventCallback struct {
	Type   string `json:"type"`
	TeamID string `json:"team_id"`
	Event  event  `json:"event"`
}

type event struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	Files       []file `json:"files"`
}

// people's subtypes: a plain message, one with files, a thread reply
// also sent to the channel. Edits, deletions, joins and bots' messages
// are no one's new words.
var people = map[string]bool{"": true, "file_share": true, "thread_broadcast": true}

// event turns a message event into a channel message. Mentions come as
// message events too (app_mention only repeats them), so only those are
// read, and each once.
func (c *Channel) event(payload json.RawMessage) (channel.Message, bool) {
	var cb eventCallback
	if json.Unmarshal(payload, &cb) != nil || cb.Type != "event_callback" || cb.TeamID != c.self.TeamID {
		return channel.Message{}, false
	}
	e := cb.Event
	if !c.fromPerson(e) {
		return channel.Message{}, false
	}
	m := channel.Message{Chat: chatOf(e.Channel, e.ThreadTS, e.TS, e.ChannelType == "im"), From: e.User, Files: c.files(e.Files)}
	m.Text, m.Addressed = c.addressed(e.Text)
	if e.ChannelType != "im" {
		c.inGroup(&m, e.ThreadTS != "")
	}
	if m.Addressed || !m.Group {
		c.marks.heard(m.Chat, e.TS) // what the bot answers gets 👀 while it works
	}
	return m, true
}

// inGroup marks a channel's message: for the bot when it mentions it or
// is in a thread the bot is in, which the bot then follows.
func (c *Channel) inGroup(m *channel.Message, inThread bool) {
	m.Group = true
	if inThread && c.threads.has(m.Chat) {
		m.Addressed = true
	}
	if m.Addressed {
		c.threads.add(m.Chat)
	}
}

// fromPerson: a message someone wrote, not the bot nor another bot, in a
// channel with a valid ID.
func (c *Channel) fromPerson(e event) bool {
	return e.Type == "message" && people[e.Subtype] && e.BotID == "" && e.User != "" && e.User != c.self.UserID && validID(e.Channel)
}

// addressed takes the bot's mentions out of text and says whether there
// were any; Slack's escapes come back to the characters they stand for.
func (c *Channel) addressed(text string) (string, bool) {
	out := c.mention.ReplaceAllString(text, "")
	return unescape(strings.TrimSpace(out)), out != text
}

// unescape undoes the three escapes Slack applies to message text.
func unescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

// chatOf is the chat of a message: the direct message itself, or the
// thread in a channel (a message out of a thread starts one).
func chatOf(ch, thread, ts string, direct bool) string {
	if direct {
		return ch
	}
	if thread == "" {
		thread = ts
	}
	if !validTS(thread) {
		return ch
	}
	return ch + "/" + thread
}

// splitChat is the channel and the thread of a chat.
func splitChat(chat string) (ch, thread string) {
	ch, thread, _ = strings.Cut(chat, "/")
	return ch, thread
}

func validID(s string) bool {
	if len(s) < 2 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func validTS(s string) bool {
	whole, frac, ok := strings.Cut(s, ".")
	return ok && digits(whole) && digits(frac)
}

func digits(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type interaction struct {
	Type string `json:"type"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container struct {
		ThreadTS string `json:"thread_ts"`
	} `json:"container"`
	Message struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	Actions []struct {
		Value string `json:"value"`
	} `json:"actions"`
}

// interaction turns a button pressed into a channel message, in the chat
// of the message that holds the button.
func (c *Channel) interaction(payload json.RawMessage) (channel.Message, bool) {
	var in interaction
	if json.Unmarshal(payload, &in) != nil || in.Type != "block_actions" || in.Team.ID != c.self.TeamID ||
		len(in.Actions) != 1 || in.User.ID == "" || !validID(in.Channel.ID) {
		return channel.Message{}, false
	}
	thread := in.Container.ThreadTS
	if thread == "" {
		thread = in.Message.ThreadTS
	}
	direct := strings.HasPrefix(in.Channel.ID, "D")
	return channel.Message{Chat: chatOf(in.Channel.ID, thread, in.Message.TS, direct), From: in.User.ID,
		Pressed: in.Actions[0].Value, Group: !direct}, true
}

type slashCommand struct {
	Command string `json:"command"`
	Text    string `json:"text"`
	User    string `json:"user_id"`
	Channel string `json:"channel_id"`
	Team    string `json:"team_id"`
}

// command turns the app's slash command into the Bridge's command:
// "/ringil new" is "/new". A command has no thread: in a channel it
// is the channel's own chat.
func (c *Channel) command(payload json.RawMessage) (channel.Message, bool) {
	var sc slashCommand
	if json.Unmarshal(payload, &sc) != nil || sc.Team != c.self.TeamID || sc.User == "" || !validID(sc.Channel) {
		return channel.Message{}, false
	}
	text := strings.TrimSpace(sc.Text)
	if text == "" {
		text = "help"
	}
	direct := strings.HasPrefix(sc.Channel, "D")
	return channel.Message{Chat: sc.Channel, From: sc.User, Text: "/" + strings.TrimPrefix(text, "/"), Group: !direct, Addressed: true}, true
}
