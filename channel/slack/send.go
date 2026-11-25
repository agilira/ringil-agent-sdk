// send.go: sending to a chat, with blocks
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package slack

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/channel"
)

// Slack's limits: a section's text, a markdown block, a button's label,
// its value, the buttons of one actions block.
const (
	maxSection  = 3000
	maxMarkdown = 10000
	maxLabel    = 75
	maxValue    = 2000
	maxButtons  = 25
)

type block map[string]any

type postIn struct {
	Channel     string  `json:"channel"`
	ThreadTS    string  `json:"thread_ts,omitempty"`
	Text        string  `json:"text"`
	Blocks      []block `json:"blocks"`
	UnfurlLinks bool    `json:"unfurl_links"`
	UnfurlMedia bool    `json:"unfurl_media"`
}

// Send delivers out to the chat, in a thread when the chat is one. Each
// part goes as its own message: the agent's markdown as a markdown block,
// the Bridge's texts as plain text (what is approved shows as it is),
// code preformatted; the buttons go under the last part. A thread the
// bot writes in is one it follows.
func (c *Channel) Send(ctx context.Context, chat string, out channel.Out) error {
	ch, thread := splitChat(chat)
	if !validID(ch) || (thread != "" && !validTS(thread)) {
		return errors.New(ErrAPI, "not a Slack chat: "+chat)
	}
	posts := compose(out)
	if len(posts) == 0 {
		return errors.New(ErrAPI, "nothing to send")
	}
	defer c.unmark(ctx, chat)
	for _, p := range posts {
		p.Channel, p.ThreadTS = ch, thread
		if err := c.call(ctx, c.bot, "chat.postMessage", p, nil); err != nil {
			return err
		}
	}
	if thread != "" {
		c.threads.add(chat)
	}
	return nil
}

// compose is out as messages, one per part, the buttons under the last.
func compose(out channel.Out) []postIn {
	var posts []postIn
	if out.Markdown {
		for _, part := range split(out.Text, maxMarkdown) {
			posts = append(posts, post(part, block{"type": "markdown", "text": defuse(part)}))
		}
	} else {
		for _, part := range split(out.Text, maxSection) {
			posts = append(posts, post(part, block{"type": "section", "text": plain(part)}))
		}
	}
	for _, part := range split(out.Code, maxSection) {
		posts = append(posts, post(part, preformatted(part)))
	}
	if len(out.Buttons) == 0 {
		return posts
	}
	if len(posts) == 0 {
		posts = append(posts, post("", nil))
	}
	last := &posts[len(posts)-1]
	last.Blocks = append(last.Blocks, actions(out.Buttons))
	return posts
}

// post is one message: its blocks, and the text notifications show.
func post(text string, b block) postIn {
	p := postIn{Text: escape(clip(text, maxSection))}
	if b != nil {
		p.Blocks = []block{b}
	}
	return p
}

func plain(text string) block {
	return block{"type": "plain_text", "text": text, "emoji": false}
}

func preformatted(code string) block {
	return block{"type": "rich_text", "elements": []block{{
		"type":     "rich_text_preformatted",
		"elements": []block{{"type": "text", "text": code}},
	}}}
}

func actions(buttons []channel.Button) block {
	elements := make([]block, 0, len(buttons))
	for i, b := range buttons {
		if i == maxButtons {
			break
		}
		elements = append(elements, block{
			"type":      "button",
			"text":      plain(clip(b.Label, maxLabel)),
			"action_id": "b" + strconv.Itoa(i),
			"value":     clip(b.ID, maxValue),
		})
	}
	return block{"type": "actions", "elements": elements}
}

// escape keeps a text from being read as Slack's markup (<!channel>,
// <@U…>, links): the three characters it reserves become entities.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// clip cuts s to at most n runes.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// split cuts text into parts of at most limit runes, at the last line
// break of a part when there is one. Blank texts give no parts.
func split(text string, limit int) []string {
	var parts []string
	for strings.TrimSpace(text) != "" {
		cut := fit(text, limit)
		if cut < len(text) {
			if nl := strings.LastIndexByte(text[:cut], '\n'); nl > 0 {
				cut = nl + 1
			}
		}
		if part := strings.TrimRight(text[:cut], "\n"); strings.TrimSpace(part) != "" {
			parts = append(parts, part)
		}
		text = text[cut:]
	}
	return parts
}

// fit is the length in bytes of the longest start of text within limit
// runes.
func fit(text string, limit int) int {
	n := 0
	for i := range text {
		if n == limit {
			return i
		}
		n++
	}
	return len(text)
}
