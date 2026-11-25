// security_test.go: fuzz and boundary tests for the Slack channel
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package slack

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Whatever the agent writes, its markdown opens none of Slack's markup:
// no <! (here, channel), <@ (a person) or <# (a channel) is left, inside
// code or out; and nothing but < is changed.
func FuzzDefuse(f *testing.F) {
	for _, s := range []string{
		"<!here> <!channel> <@U1> <#C1>", "`<!here>`", "```\n<!channel>\n```", "``a`b`` <@U1>",
		"```\n<!here>", "~~~\n<@U1>\n~~~", "a<b && c>d `Vec<T>`", "\\<!here>", "<<!here>", "`` ` <!here> ` ``",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, md string) {
		out := defuse(md)
		for _, open := range []string{"<!", "<@", "<#"} {
			if strings.Contains(out, open) {
				t.Fatalf("%q kept %s: %q", md, open, out)
			}
		}
		if strings.ReplaceAll(out, "&lt;", "<") != strings.ReplaceAll(md, "&lt;", "<") {
			t.Fatalf("%q changed beyond <: %q", md, out)
		}
	})
}

var chatForm = regexp.MustCompile(`^[A-Z0-9]{2,32}(/[0-9]{1,20}\.[0-9]{1,20})?$`)

// An event of any shape gives no message, or one whose chat is a valid
// Slack chat and whose sender is someone else than the bot.
func FuzzEvent(f *testing.F) {
	f.Add(`{"type":"event_callback","team_id":"T1","event":{"type":"message","channel":"C1","channel_type":"channel","user":"U1","text":"<@UBOT> hi","ts":"1.2","thread_ts":"1.1"}}`)
	f.Add(`{"type":"event_callback","team_id":"T1","event":{"type":"message","channel":"C1/../x","user":"U1","ts":"1.2"}}`)
	f.Add(`{"type":"event_callback","team_id":"T1","event":{"type":"message","channel":"D1","channel_type":"im","user":"UBOT","ts":"x"}}`)
	c := &Channel{self: identity{UserID: botUser, TeamID: team}, threads: newRecent(8), marks: newMarks(), fileHost: "files.slack.com"}
	c.mention = regexp.MustCompile(`<@` + botUser + `(\|[^>]*)?>`)
	f.Fuzz(func(t *testing.T, payload string) {
		m, ok := c.event(json.RawMessage(payload))
		if !ok {
			return
		}
		if !chatForm.MatchString(m.Chat) || m.From == "" || m.From == botUser {
			t.Fatalf("%s gave %+v", payload, m)
		}
	})
}

// The bot token goes to files.slack.com over https only.
func TestFileHostBoundaries(t *testing.T) {
	for _, u := range []string{
		"http://files.slack.com/x", "https://files.slack.com.evil.com/x", "https://evil.com/files.slack.com/x",
		"https://user@files.slack.com/x", "https://files.slack.com:8443/x", "//files.slack.com/x", "",
	} {
		if fileURL(u, "files.slack.com") {
			t.Errorf("%q accepted", u)
		}
	}
	if !fileURL("https://files.slack.com/files-pri/T1-F1/download/a.pdf", "files.slack.com") {
		t.Error("Slack's own file address refused")
	}
}

// Only wss reaches the socket, but ws to loopback (a test).
func TestSocketURLBoundaries(t *testing.T) {
	for _, u := range []string{"ws://wss-primary.slack.com/link", "https://x", "wss://u:p@x/link", "ws://10.0.0.1/x"} {
		if checkSocketURL(u) == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if checkSocketURL("wss://wss-primary.slack.com/link/?ticket=1") != nil || checkSocketURL("ws://127.0.0.1:1/ws") != nil {
		t.Error("a good address refused")
	}
}
