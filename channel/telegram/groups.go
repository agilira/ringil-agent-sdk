// groups.go: group chats, and who belongs to a group
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package telegram

import (
	"context"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"
)

func isGroup(m *message) bool {
	return m.Chat.Type == "group" || m.Chat.Type == "supergroup"
}

// addressed says whether a group's message is for the bot (a command for
// it, its @name, or an answer to one of its messages) and returns the
// text without its @name.
func (c *Channel) addressed(m *message, text string) (string, bool) {
	if c.bot.Username == "" {
		return text, m.ReplyTo != nil && m.ReplyTo.From != nil && m.ReplyTo.From.ID == c.bot.ID
	}
	named := c.name.MatchString(text)
	clean := strings.TrimSpace(c.name.ReplaceAllString(text, ""))
	switch {
	case named, m.ReplyTo != nil && m.ReplyTo.From != nil && m.ReplyTo.From.ID == c.bot.ID:
		return clean, true
	case strings.HasPrefix(text, "/") && !strings.Contains(strings.Fields(text)[0], "@"):
		return text, true // a command for every bot of the group
	}
	return text, false
}

// MemberOf returns a check that a sender belongs to a Telegram group, as
// channel.Config.IsMember wants: the company's group, say, whose people
// are its members. The bot must be in that group.
func (c *Channel) MemberOf(group string) func(ctx context.Context, from string) (bool, error) {
	return func(ctx context.Context, from string) (bool, error) {
		if _, err := strconv.ParseInt(from, 10, 64); err != nil {
			return false, errors.New(ErrAPI, "not a Telegram user ID")
		}
		var m struct {
			Status   string `json:"status"`
			IsMember bool   `json:"is_member"`
		}
		if err := c.call(ctx, "getChatMember", map[string]string{"chat_id": group, "user_id": from}, &m); err != nil {
			return false, err
		}
		switch m.Status {
		case "creator", "administrator", "member":
			return true, nil
		case "restricted":
			return m.IsMember, nil
		}
		return false, nil
	}
}
