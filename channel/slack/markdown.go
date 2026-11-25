// markdown.go: the agent's markdown, kept from pinging anyone
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package slack

import "strings"

// special are the openings of Slack's markup: a mention of everyone, of
// a person, of a channel.
var special = strings.NewReplacer("<!", "&lt;!", "<@", "&lt;@", "<#", "&lt;#")

// defuse keeps the agent's markdown from being read as Slack's markup:
// in a markdown block <!here>, <!channel> and <@U…> are live mentions,
// so every < outside code becomes &lt;, which shows as <. Inside code
// (a fenced block or a `span`) Slack shows &lt; as written, so there only
// the three openings of Slack's markup are escaped (<! <@ <#), and a <
// of code (a<b, Vec<T>, <div>) stays: should Slack read a fence otherwise
// than here, no mention gets through either way.
func defuse(md string) string {
	var b strings.Builder
	fenced := false
	for _, line := range strings.SplitAfter(md, "\n") {
		if fence(line) || fenced {
			fenced = fenced != fence(line)
			b.WriteString(special.Replace(line))
			continue
		}
		b.WriteString(defuseLine(line))
	}
	return b.String()
}

// fence: the line opens or closes a fenced code block.
func fence(line string) bool {
	t := strings.TrimLeft(line, " ")
	return len(line)-len(t) <= 3 && (strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~"))
}

// defuseLine escapes < outside the line's code spans. A span opens with a
// run of backticks and closes with a run of the same length; an unclosed
// run is text.
func defuseLine(line string) string {
	var b strings.Builder
	for line != "" {
		i := strings.IndexAny(line, "<`")
		if i < 0 {
			b.WriteString(line)
			break
		}
		b.WriteString(line[:i])
		if line[i] == '<' {
			b.WriteString("&lt;")
			line = line[i+1:]
			continue
		}
		n := run(line[i:])
		end := closing(line[i+n:], n)
		if end < 0 {
			b.WriteString(line[i : i+n]) // no closing run: plain backticks
			line = line[i+n:]
			continue
		}
		b.WriteString(special.Replace(line[i : i+n+end+n]))
		line = line[i+n+end+n:]
	}
	return b.String()
}

// run is the length of the backtick run s starts with.
func run(s string) int {
	n := 0
	for n < len(s) && s[n] == '`' {
		n++
	}
	return n
}

// closing is where a run of exactly n backticks starts in s, or -1.
func closing(s string, n int) int {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		m := run(s[i:])
		if m == n {
			return i
		}
		i += m
	}
	return -1
}
