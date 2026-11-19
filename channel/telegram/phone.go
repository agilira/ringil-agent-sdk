// phone.go: asking a user for their phone number
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package telegram

import "context"

// contact is a contact sent in a chat: a phone number and, when it is a
// Telegram user's, that user.
type contact struct {
	Phone  string `json:"phone_number"`
	UserID int64  `json:"user_id"`
}

// phone is the sender's own phone number, when the message shares it in
// a private chat: a contact whose user is the sender, as Telegram puts
// it. Anyone can send a contact card with any number; only Telegram sets
// its user.
func phone(m *message) string {
	if m.Contact == nil || m.Chat.Type != "private" || m.Contact.UserID == 0 || m.Contact.UserID != m.From.ID {
		return ""
	}
	return m.Contact.Phone
}

// replyKeyboard is the keyboard under the field where the user writes:
// here one button, which shares their number.
type replyKeyboard struct {
	Keyboard [][]contactButton `json:"keyboard"`
	OneTime  bool              `json:"one_time_keyboard"`
	Resize   bool              `json:"resize_keyboard"`
}

type contactButton struct {
	Text    string `json:"text"`
	Contact bool   `json:"request_contact"`
}

// noKeyboard takes the reply keyboard away.
type noKeyboard struct {
	Remove bool `json:"remove_keyboard"`
}

// AskPhone sends text with a button that shares the user's phone number
// (in a private chat: Telegram shows it nowhere else). The next text
// without buttons sent to the chat after a contact comes takes the
// button away.
func (c *Channel) AskPhone(ctx context.Context, chat, text string) error {
	in := sendIn{Chat: chat, Text: text, Markup: replyKeyboard{
		Keyboard: [][]contactButton{{{Text: "Share my phone number", Contact: true}}}, OneTime: true, Resize: true}}
	return c.call(ctx, "sendMessage", in, nil)
}

// shared notes that a chat shared a contact: its next text takes the
// keyboard away.
func (c *Channel) shared(chat string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.unkey) >= maxUnkey {
		clear(c.unkey) // the keyboard only stays a while longer
	}
	c.unkey[chat] = true
}

// unkeyed says whether a chat's keyboard is to be taken away now, and
// forgets it.
func (c *Channel) unkeyed(chat string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.unkey[chat]
	delete(c.unkey, chat)
	return was
}

// maxUnkey bounds the chats whose keyboard is still to be taken away.
const maxUnkey = 10000
