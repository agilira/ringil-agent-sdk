// listen.go: what the host's message triggers hear
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"strings"
)

// ChatKey is a chat's key for the host, the same for all its
// conversations (ChatOf one of them): what triggers name a chat by, to
// listen to it or deliver to it.
func ChatKey(channel, chatID string) string {
	return ChatOf(Conversation(channel, chatID, 1))
}

// overhear passes a message to the host's listeners (Config.Hear): only
// written words, not commands, only of an owner or a member known as
// one, only in a chat listened to.
func (b *Bridge) overhear(m Message) {
	text := strings.TrimSpace(m.Text)
	if b.cfg.Hear == nil || b.cfg.Listens == nil || m.Pressed != "" || text == "" || strings.HasPrefix(text, "/") {
		return
	}
	chat := ChatKey(b.cfg.Channel.Name(), m.Chat)
	if b.cfg.Listens(chat) && b.known(m.From) {
		b.cfg.Hear(chat, m.Text)
	}
}
