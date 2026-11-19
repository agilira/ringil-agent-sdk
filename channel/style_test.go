// style_test.go: how an agent talks in a chat app
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import "testing"

func TestOwns(t *testing.T) {
	for conv, want := range map[string]bool{
		Conversation("telegram", "42", 1): true, Conversation("telegram", "42", 3): true,
		Conversation("telegram", "a b", 1): true, Conversation("slack", "42", 1): false,
		"telegramx-42": false, "chat-1": false, "": false,
	} {
		if Owns("telegram", conv) != want {
			t.Errorf("%q", conv)
		}
	}
}
