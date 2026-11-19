// split.go: long texts in parts Telegram takes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package telegram

import (
	"strings"
	"unicode/utf16"
)

// maxText is Telegram's limit for a message, in UTF-16 code units.
const maxText = 4096

// split cuts text into parts of at most limit UTF-16 code units, at the
// last line break of a part when there is one, else at the limit. Blank
// texts give no parts.
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
// UTF-16 code units, cut between runes.
func fit(text string, limit int) int {
	units := 0
	for i, r := range text {
		n := utf16.RuneLen(r)
		if n < 0 {
			n = 1 // invalid UTF-8: one replacement character
		}
		if units+n > limit {
			return i
		}
		units += n
	}
	return len(text)
}
