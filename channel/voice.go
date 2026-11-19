// voice.go: answering voice with voice
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// maxSpoken bounds an answer spoken: longer ones are read better.
const maxSpoken = 4000

// speakTimeout bounds the speaking of an answer: past it, text.
var speakTimeout = time.Minute

// canSpeak: a synthesizer, and a channel that sends voice.
func (b *Bridge) canSpeak() bool {
	_, ok := b.cfg.Channel.(VoiceSender)
	return ok && b.cfg.Synthesizer != nil
}

// speaks says whether the chat's voice messages are answered with voice:
// its /voice or /text, else the host's choice.
func (b *Bridge) speaks(chatID string) bool {
	b.mu.Lock()
	on, set := b.voice[chatID]
	b.mu.Unlock()
	if !set {
		on = b.cfg.VoiceReplies
	}
	return on && b.canSpeak()
}

func (b *Bridge) setVoice(ctx context.Context, chatID string, on bool) {
	if on && !b.canSpeak() {
		b.say(ctx, chatID, b.tr(chatID, mNoVoiceOut))
		return
	}
	b.mu.Lock()
	if len(b.voice) >= maxTracked {
		clear(b.voice) // the chats go back to the host's choice
	}
	b.voice[chatID] = on
	b.mu.Unlock()
	if on {
		b.say(ctx, chatID, b.tr(chatID, mVoiceOn))
	} else {
		b.say(ctx, chatID, b.tr(chatID, mVoiceOff))
	}
}

// speak is text spoken for the chat, or nil when the chat wants text,
// the text does not suit speech (long, or with code), or speaking fails.
func (b *Bridge) speak(ctx context.Context, chatID, text string) *Audio {
	if !b.speaks(chatID) || utf8.RuneCountInString(text) > maxSpoken || strings.Contains(text, "```") {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, speakTimeout)
	defer cancel()
	data, mime, err := b.cfg.Synthesizer.Synthesize(ctx, plain(text))
	if err != nil {
		if ctx.Err() != context.Canceled { // a /stop is not a failure
			b.report(Event{Kind: VoiceFailed, Chat: chatID, Err: err})
		}
		return nil
	}
	return &Audio{MIME: mime, Data: data}
}

// plain is text without the Markdown a voice would read aloud: heading,
// quote and list marks at the start of lines, emphasis in pairs, code
// marks, and links as their text. Nothing else changes ("-5 °C", "2*3").
func plain(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = lineMark.ReplaceAllString(l, "")
	}
	text = strings.Join(lines, "\n")
	text = link.ReplaceAllString(text, "$1")
	for _, e := range emphasis {
		text = e.ReplaceAllString(text, "$1$2$3")
	}
	return strings.ReplaceAll(text, "`", "")
}

var (
	lineMark = regexp.MustCompile(`^\s*(#{1,6}|>|[-*+]|\d+[.)])\s+`)
	link     = regexp.MustCompile(`\[([^\]]+)\]\([^)\s]+\)`)
	// emphasis: a mark pair around words, not inside a word ("2*3*4").
	emphasis = []*regexp.Regexp{marks(`\*\*`), marks(`__`), marks(`\*`), marks(`_`)}
)

func marks(m string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^\pL\pN*_])` + m + `(\S|\S[^*_\n]*?\S)` + m + `($|[^\pL\pN*_])`)
}

func hasVoice(files []File) bool {
	for _, f := range files {
		if f.Voice {
			return true
		}
	}
	return false
}
