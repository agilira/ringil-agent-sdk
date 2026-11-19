// style.go: how an agent talks in a chat app
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import "strings"

// Style is what an agent should know to talk well in a chat app, for the
// host to add to the system prompt of the profiles its chats talk with:
// the Bridge sends what the agent writes as it is.
const Style = "You are talking in a chat app, often on a phone. Write as a person texting: short, the answer or the result first, " +
	"in the user's language. No headings or tables; bold and lists only when they really help. " +
	"Do not repeat what the user asked, do not list what you could do next, and do not end with offers of help: " +
	"ask a question only when you cannot go on without the answer. " +
	"Files the user sends come with where they were saved; read them with your tools when the request needs it."

// Owns says whether a conversation is one of the channel name's chats
// (see Conversation).
func Owns(name, conversation string) bool {
	return validName.MatchString(name) && ChatOf(conversation) != "" &&
		(strings.HasPrefix(conversation, name+"-") || strings.HasPrefix(conversation, name+"_h"))
}
