// format.go: the agent's markdown as Telegram shows it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package telegram

import (
	"context"
	"html"
	"regexp"
	"strings"

	"github.com/agilira/ringil-agent-sdk/channel"
)

// Models write markdown; Telegram shows it as written (stars and
// backticks) unless sent as its HTML. format turns the common marks into
// it: code blocks and code, bold, italics with *, strikethrough, links,
// headings as bold, list marks as bullets. Everything else is escaped, so
// the text never makes a tag of its own.
func format(md string) string {
	var b strings.Builder
	for i, piece := range fence.Split(md, -1) {
		if i > 0 {
			b.WriteString(block(fence.FindAllStringSubmatch(md, -1)[i-1][1]))
		}
		b.WriteString(inline(piece))
	}
	return b.String()
}

var (
	fence    = regexp.MustCompile("(?s)```[A-Za-z0-9_+-]*\n(.*?)\n?```")
	codeSpan = regexp.MustCompile("`([^`\n]+)`")
	heading  = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t#]*$`)
	bullet   = regexp.MustCompile(`(?m)^([ \t]*)[-*+][ \t]+`)
	bold     = regexp.MustCompile(`\*\*(\S|\S[^\n]*?\S)\*\*`)
	italic   = regexp.MustCompile(`(^|[^\pL\pN*])\*(\S|\S[^*\n]*?\S)\*($|[^\pL\pN*])`)
	strike   = regexp.MustCompile(`~~(\S|\S[^\n]*?\S)~~`)
	mdLink   = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
)

func block(code string) string {
	return "<pre>" + html.EscapeString(code) + "</pre>"
}

// inline formats text outside code blocks: code spans as they are, the
// rest with its marks.
func inline(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range codeSpan.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(marks(text[last:m[0]]))
		b.WriteString("<code>" + html.EscapeString(text[m[2]:m[3]]) + "</code>")
		last = m[1]
	}
	b.WriteString(marks(text[last:]))
	return b.String()
}

func marks(text string) string {
	s := html.EscapeString(text) // marks and brackets are not escaped: they still match
	s = heading.ReplaceAllString(s, "<b>$1</b>")
	s = bullet.ReplaceAllString(s, "$1• ")
	s = bold.ReplaceAllString(s, "<b>$1</b>")
	s = italic.ReplaceAllString(s, "$1<i>$2</i>$3")
	s = strike.ReplaceAllString(s, "<s>$1</s>")
	return mdLink.ReplaceAllString(s, `<a href="$2">$1</a>`)
}

// sendCode sends a text with its code in one message, the code in
// monospace; false, sending nothing, when it does not fit in one.
func (c *Channel) sendCode(ctx context.Context, chat string, out channel.Out) (bool, error) {
	text := html.EscapeString(strings.TrimRight(out.Text, "\n"))
	if out.Markdown {
		text = format(out.Text)
	}
	in := sendIn{Chat: chat, Text: text + "\n<pre>" + html.EscapeString(out.Code) + "</pre>", Parse: "HTML"}
	if fit(in.Text, maxText) < len(in.Text) {
		return false, nil
	}
	if len(out.Buttons) > 0 {
		in.Markup = keyboard(out.Buttons)
	} else if c.unkeyed(chat) {
		in.Markup = noKeyboard{Remove: true}
	}
	return true, c.call(ctx, "sendMessage", in, nil)
}
