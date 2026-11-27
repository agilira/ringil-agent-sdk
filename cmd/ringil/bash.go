// bash.go: the bash tool in the CLI
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/tools/bash"
)

// bashTool runs commands in the first workspace directory; the others
// are readable. Network domains are allowed by --allow-domains or asked
// about; commands outside the sandbox and dangerous ones are confirmed.
func (c *cli) bashTool(ctx *orpheus.Context, dirs []string) ([]agent.Tool, error) {
	box, err := c.newSandbox()
	if err != nil {
		box = nil // the tool finds none either: every command is confirmed
	}
	cfg := bash.Config{
		Dir:     dirs[0],
		Sandbox: box,
		Read:    append(bash.ReadDefaults(), dirs[1:]...),
		Network: bash.Network{Allow: splitList(c.flagString(ctx, "allow-domains")), Ask: c.askDomain},
		Confirm: c.confirmCommand,
		// ringil.yaml read-only in the sandbox too, as the file tools
		// confirm it.
		Protected: protectedPatterns,
	}
	if base, err := c.configDir(); err == nil {
		cfg.Never = []string{filepath.Join(base, "ringil")}
	}
	sh, err := bash.New(cfg)
	if err != nil {
		return nil, err
	}
	// Its background commands end with the session or the server's run,
	// checked (audit 21/11: they outlived both).
	c.closers = append(c.closers, sh.Close)
	return sh.Tools(), nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// confirmCommand asks about a command outside the sandbox or a dangerous
// one: in the terminal, or under serve through the API.
func (c *cli) confirmCommand(ctx context.Context, req bash.Request) (bool, error) {
	if _, decided := c.triggerAllowed("bash"); decided {
		return false, nil // a trigger's runs run commands in the sandbox only: no one sees the others
	}
	if c.unasked() {
		return true, nil
	}
	if c.server != nil {
		input, err := json.Marshal(map[string]any{"command": req.Command, "reason": req.Reason, "why": req.Why, "explanation": req.Explanation})
		if err != nil {
			return false, err
		}
		return c.server.Ask(ctx, agent.ToolCall{ID: "bash-" + rand.Text()[:8], Name: "bash_" + string(req.Reason), Input: input})
	}
	c.holdTerminal(ctx)
	defer c.askMu.Unlock()
	q := "\n  ! " + asker(ctx) + " wants to run:\n      " + clean(req.Command) + "\n"
	if req.Reason == bash.Unsandboxed {
		q += "    outside the sandbox, with your rights"
		if req.Explanation != "" {
			q += " (" + clean(req.Explanation) + ")"
		}
		q += ".\n"
	}
	for _, w := range req.Why {
		q += "    " + w + ".\n"
	}
	return c.confirm(q + "  Allow? [y/N] ")
}

// askDomain asks whether bash commands may reach a domain, once per run.
func (c *cli) askDomain(ctx context.Context, host string) (bool, error) {
	if _, decided := c.triggerAllowed(""); decided {
		return false, nil // a trigger's run reaches only --allow-domains
	}
	if c.unasked() {
		return true, nil
	}
	if c.server != nil {
		input, err := json.Marshal(map[string]string{"domain": host})
		if err != nil {
			return false, err
		}
		return c.server.Ask(ctx, agent.ToolCall{ID: "net-" + rand.Text()[:8], Name: "network_domain", Input: input})
	}
	c.holdTerminal(ctx)
	defer c.askMu.Unlock()
	return c.confirm("\n  ! A command run by " + strings.ToLower(asker(ctx)) + " wants to reach " + clean(host) + " — allow it for this session? [y/N] ")
}
