// project.go: /project, an owner makes a project from a chat
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"strings"
)

// Project is the source (serve.SourceOf) of the runs /project starts: the
// owner asked to make a project of what they say. The host's agent makes
// it with its project tool, showing it before it is made. A project is
// never made from other words: most work is a one-off.
const Project = "project"

// projectCommand reads "/project what" (or /project@Bot what).
func projectCommand(text string) (string, bool) {
	text = strings.TrimSpace(text)
	first, rest := text, ""
	if i := strings.IndexAny(text, " \n\t"); i >= 0 {
		first, rest = text[:i], text[i+1:]
	}
	word, _, _ := strings.Cut(strings.ToLower(first), "@")
	if word != "/project" {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// project handles /project: an owner, in their own chat, starts a run
// that makes a project of what they say; false when text is not the
// command.
func (b *Bridge) project(ctx context.Context, m Message, r role, text string) bool {
	what, ok := projectCommand(text)
	switch {
	case !ok:
		return false
	case r != owner || m.Group:
		b.say(ctx, m.Chat, b.tr(m.Chat, mProjectOwner))
	case what == "":
		b.say(ctx, m.Chat, b.tr(m.Chat, mProjectHelp))
	default:
		m.Text = "Make a project of this: " + what
		b.startFrom(ctx, m, r, Project)
	}
	return true
}
