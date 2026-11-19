// automate.go: /automate, the one way a chat makes automations
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"strings"
)

// Automate is the source (serve.SourceOf) of the runs /automate starts:
// the host gives them, and only them, the tools that make automations
// (create_trigger). Anything else said in a chat is the conversation's
// work, however often it says "every": an automation is never made by
// mistake.
const Automate = "automate"

// automation reads "/automate what" (or /automate@Bot what): what, and
// whether text is that command.
func automation(text string) (string, bool) {
	text = strings.TrimSpace(text)
	first, rest := text, ""
	if i := strings.IndexAny(text, " \n\t"); i >= 0 {
		first, rest = text[:i], text[i+1:]
	}
	word, _, _ := strings.Cut(strings.ToLower(first), "@")
	if word != "/automate" {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// automate handles /automate: an owner, in their own chat, starts a run
// that turns what they say into an automation; false when text is not
// the command.
func (b *Bridge) automate(ctx context.Context, m Message, r role, text string) bool {
	what, ok := automation(text)
	switch {
	case !ok:
		return false
	case r != owner || m.Group:
		b.say(ctx, m.Chat, b.tr(m.Chat, mAutomateOwner))
	case what == "":
		b.say(ctx, m.Chat, b.tr(m.Chat, mAutomateHelp))
	default:
		m.Text = "Make this automation: " + what
		b.startFrom(ctx, m, r, Automate)
	}
	return true
}
