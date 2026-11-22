// onboard_composio.go: Composio in the guided setup
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"
	"go.yaml.in/yaml/v3"

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
	if err := s.keys.Set(s.ctx, composioKey, key); err != nil {
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
func (s *setup) composioSection(servers *yaml.Node) {
	name := "composio"
	for i := 0; i+1 < len(servers.Content); i += 2 {
		if e := servers.Content[i+1]; e.Kind == yaml.MappingNode && isComposio(scalarAt(e, "url")) {
			name = servers.Content[i].Value
			break
		}
	}
	srv := mapAt(servers, name)
	if base, err := s.c.configDir(); err == nil && mapHas(srv, "oauth") { // where withOAuth keeps it
		_ = os.Remove(filepath.Join(base, "ringil", "oauth", name+".json")) // #nosec G104 -- none is fine
	}
	for _, k := range []string{"oauth", "disabled", "command", "args", "env", "sandbox", "network", "mounts"} {
		removeKey(srv, k)
	}
	merge(srv, "type", "http", "url", composio.ConnectURL)
	headers := mapAt(srv, "headers")
	for i := len(headers.Content) - 2; i >= 0; i -= 2 {
		if strings.EqualFold(headers.Content[i].Value, "x-consumer-api-key") {
			headers.Content = append(headers.Content[:i], headers.Content[i+2:]...)
		}
	}
	setKey(headers, "x-consumer-api-key", scalar("${"+composioKey+"}"))
}

func isComposio(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(u.Hostname(), "connect.composio.dev")
}

// scalarAt is the scalar under key, or "".
func scalarAt(m *yaml.Node, key string) string {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.ScalarNode {
			return m.Content[i+1].Value
		}
	}
	return ""
}

func mapHas(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return true
		}
	}
	return false
}
