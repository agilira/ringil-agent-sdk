// onboard_composio.go: Composio in the guided setup
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/mcp"
	"github.com/agilira/ringil-agent-sdk/mcp/composio"
)

// composioKey is the name the consumer key is kept under in the store;
// ringil.yaml refers to it as ${COMPOSIO_KEY}.
const composioKey = "COMPOSIO_KEY"

// askComposio connects Composio, if wanted: the consumer key (ck_...) is
// tried against Composio Connect, kept in the store, and the server is
// written to mcp_servers. The apps (Gmail, Slack...) are those already
// linked at composio.dev; others are linked from the chat, by asking.
func (s *setup) askComposio() error {
	yes, err := s.p.Confirm("Use Composio, for your apps (Gmail, Slack, GitHub...)? It needs a consumer key (ck_...) from composio.dev", false)
	if err != nil || !yes {
		return err
	}
	for range 3 {
		key, err := s.p.AskSecret("Your Composio consumer key (not shown, kept in the store you chose)")
		if err != nil {
			return err
		}
		again, err := s.keepComposio(key)
		if !again {
			return err
		}
	}
	return s.c.line("  Composio refused the key three times: skipped (run `ringil setup` again when you have it)")
}

// keepComposio tries the key and keeps it; again asks for another, when
// Composio refused this one.
func (s *setup) keepComposio(key string) (again bool, err error) {
	tools, err := s.c.checkComposio(s.ctx, key)
	if errors.HasCode(err, mcp.ErrRefused) {
		return true, s.c.line("  Composio refused the key: check it and paste it again")
	}
	said := "  Composio answered with " + strconv.Itoa(tools) + " tools; link more apps from the chat, by asking"
	if err != nil { // not refused, not checked
		keep, cerr := s.p.Confirm("Composio could not be reached ("+describe(err)+"). Keep the key anyway?", true)
		if cerr != nil {
			return false, cerr
		}
		if !keep { // Composio is optional: the rest of the setup goes on
			return false, s.c.line("  Composio skipped")
		}
		said = "  the key is kept; Composio is tried again when ringil starts"
	}
	if err := s.keep(composioKey, key); err != nil {
		return false, err
	}
	s.composio = true
	return false, s.c.line(said)
}

// connectComposio tries a consumer key: the number of tools Composio
// offers with it.
func connectComposio(ctx context.Context, key string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := mcp.Connect(ctx, composio.Connect(func(context.Context) (string, error) { return key, nil }))
	if err != nil {
		return 0, err
	}
	n := len(c.Tools())
	_ = c.Close() // #nosec G104 -- the tools were read
	return n, nil
}

// composioSection writes the Composio server: the entry already pointing
// at Composio Connect, whatever its name, or a new "composio". What the
// key replaces goes (audit 22/11): a browser sign-in and its token, a
// local command, the same header in another case (Go would send either).
func (s *setup) composioSection(e *edit) {
	servers, _ := e.w.GetValue("mcp_servers").(map[string]interface{})
	name := composioEntry(servers)
	for n, entry := range servers {
		if m, ok := entry.(map[string]interface{}); ok && strings.Contains(n, ".") && isComposio(stringOf(m["url"])) {
			s.c.note("MCP server " + n + " also reaches Composio, but a name with a dot cannot be edited: remove it by hand, the key is under \"composio\"")
		}
	}
	at := "mcp_servers." + name
	entry, _ := servers[name].(map[string]interface{})
	if _, signedIn := entry["oauth"]; signedIn {
		s.dropOAuth = name
	}
	for _, k := range []string{"oauth", "disabled", "command", "args", "env", "sandbox", "network", "mounts"} {
		e.set(at+"."+k, nil)
	}
	e.merge(at, "type", "http", "url", composio.ConnectURL)
	headers, _ := e.w.GetValue(at + ".headers").(map[string]interface{})
	for h := range headers {
		if strings.EqualFold(h, "x-consumer-api-key") {
			e.set(at+".headers."+h, nil)
		}
	}
	e.set(at+".headers.x-consumer-api-key", "${"+composioKey+"}")
}

// dropOAuthToken removes the browser sign-in's token the key replaces,
// where withOAuth keeps it, only after ringil.yaml has been written.
func (s *setup) dropOAuthToken() {
	if s.dropOAuth == "" {
		return
	}
	if base, err := s.c.configDir(); err == nil {
		_ = os.Remove(filepath.Join(base, "ringil", "oauth", s.dropOAuth+".json")) // #nosec G104 -- none is fine
	}
}

// composioEntry is the name of the server already at Composio Connect,
// else "composio" (a name with a dot cannot be addressed: a new one).
func composioEntry(servers map[string]interface{}) string {
	for _, n := range slices.Sorted(maps.Keys(servers)) {
		if entry, ok := servers[n].(map[string]interface{}); ok && !strings.Contains(n, ".") && isComposio(stringOf(entry["url"])) {
			return n
		}
	}
	return "composio"
}

func isComposio(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(u.Hostname(), "connect.composio.dev")
}

func stringOf(v interface{}) string {
	s, _ := v.(string)
	return s
}
