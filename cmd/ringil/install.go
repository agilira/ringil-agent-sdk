// install.go: install_skill and add_mcp_server, saving into ringil.yaml
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"sort"

	"github.com/agilira/argus"
	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/mcp"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/skills"
)

// installTools are install_skill and add_mcp_server, for the user's own
// sessions: a restricted role and a trigger's runs install nothing.
func (c *cli) installTools(ctx *orpheus.Context, cat *skills.Catalog) []agent.Tool {
	dir, err := c.skillsDir()
	if err != nil || c.restricted() || c.inTrigger {
		return nil
	}
	resolve := func() (secrets.Resolver, error) { return c.resolver(ctx) }
	return []agent.Tool{
		skills.InstallTool(dir, skills.GitHub{}, func(skills.Installed) { c.reloadSkills(cat) }),
		mcp.AddTool(func(ctx context.Context, e mcp.Entry) (mcp.Added, error) { return c.addMCP(ctx, e, resolve) }),
	}
}

// reloadSkills reads the catalog again: the skill just installed is
// loaded and run in this session, and listed from the next prompt.
func (c *cli) reloadSkills(cat *skills.Catalog) {
	if cat == nil {
		return
	}
	if err := cat.Reload(); err != nil {
		c.note("the skills could not be read again: " + describe(err))
	}
}

// entryOf is an added server as ringil.yaml keeps it. A remote one
// without headers signs in through the browser if the server asks.
func entryOf(e mcp.Entry) mcpEntry {
	m := mcpEntry{Env: e.Env, URL: e.URL, Headers: e.Headers, Source: e.Source}
	if len(e.Command) > 0 {
		m.Command, m.Args = e.Command[0], e.Command[1:]
	}
	if e.URL != "" && len(e.Headers) == 0 {
		m.OAuth = &oauthEntry{}
	}
	return m
}

// Outcomes of adding an MCP server beside mcp's codes: the name is taken;
// the server is saved but its tools come in the next session.
const (
	errMCPExists      errors.ErrorCode = "MCP_EXISTS"
	errMCPNextSession errors.ErrorCode = "MCP_NEXT_SESSION"
)

// fitsOnDisk checks ringil.yaml as it is on disk with the server added,
// as every change is checked (no file watched: nothing to check against).
// The caller holds keepMu.
func (c *cli) fitsOnDisk(name string, e mcpEntry) error {
	if c.settings == nil || c.settings.path == "" {
		return nil
	}
	cur, err := c.settings.onDisk(c)
	if err != nil {
		return agent.ToolError(mcp.ErrConfig, "ringil.yaml on disk is not valid, fix it first: "+describe(err))
	}
	next := cur.clone()
	if next.MCPServers == nil {
		next.MCPServers = map[string]mcpEntry{}
	}
	next.MCPServers[name] = e
	if err := next.Validate(); err != nil {
		return agent.ToolError(mcp.ErrConfig, "The server cannot be added: "+describe(err))
	}
	return nil
}

// addMCP saves the server under mcp_servers and, when its secrets are
// all set, starts it and adds its tools to this session's.
func (c *cli) addMCP(ctx context.Context, e mcp.Entry, resolve func() (secrets.Resolver, error)) (mcp.Added, error) {
	if _, taken := c.conf().MCPServers[e.Name]; taken {
		return mcp.Added{}, agent.ToolError(errMCPExists, "A server named "+e.Name+" is already configured: pick another name.")
	}
	res, err := resolve()
	if err != nil {
		return mcp.Added{}, err
	}
	entry := entryOf(e)
	missing, err := missingRefs(ctx, res, e.References())
	if err != nil {
		return mcp.Added{}, err
	}
	if err := c.saveServer(e.Name, entry); err != nil {
		return mcp.Added{}, err
	}
	if len(missing) > 0 {
		return mcp.Added{Missing: missing, NextSession: true}, nil
	}
	return c.attachServer(ctx, e.Name, entry, res)
}

// missingRefs are the names res does not hold.
func missingRefs(ctx context.Context, res secrets.Resolver, names []string) ([]string, error) {
	var missing []string
	for _, n := range names {
		_, found, err := secrets.Lookup(ctx, res, n)
		if err != nil {
			return nil, errors.Wrap(err, ErrUsage, "cannot resolve ${"+n+"}")
		}
		if !found {
			missing = append(missing, n)
		}
	}
	return missing, nil
}

// attachServer starts the server and adds its tools to the session's;
// without a live session (a test) it lists them and stops it.
func (c *cli) attachServer(ctx context.Context, name string, e mcpEntry, res secrets.Resolver) (mcp.Added, error) {
	client, err := c.startServer(ctx, name, e, res)
	if err != nil {
		return mcp.Added{}, agent.ToolError(mcp.ErrConnect, "The server is saved in ringil.yaml but did not start: check its command or URL with the user. ("+describe(err)+")")
	}
	tools := client.Tools()
	added := mcp.Added{NextSession: c.live == nil}
	for _, t := range tools {
		added.Tools = append(added.Tools, t.Spec().Name)
	}
	sort.Strings(added.Tools)
	if c.live == nil {
		return added, client.Close()
	}
	if err := c.live.Add(tools...); err != nil {
		_ = client.Close()
		return mcp.Added{NextSession: true}, agent.ToolError(errMCPNextSession, "The server is saved, but a tool name clashes with one in use: it comes in the next session.")
	}
	c.closers = append(c.closers, client.Close)
	return added, nil
}

// saveServer writes the entry under mcp_servers.<name> through argus's
// writer, the rest of the file untouched, after a backup.
func (c *cli) saveServer(name string, e mcpEntry) error {
	c.keepMu.Lock()
	defer c.keepMu.Unlock()
	path := c.configName()
	w, err := argus.NewConfigWriter(path, argus.FormatYAML, nil)
	if err != nil {
		return errors.Wrap(err, ErrUsage, path+" cannot be edited")
	}
	if w.GetValue("mcp_servers."+name) != nil { // on disk: one saved since this session read the file
		return agent.ToolError(errMCPExists, "A server named "+name+" is already configured: pick another name.")
	}
	if err := c.fitsOnDisk(name, e); err != nil {
		return err
	}
	ed := &edit{w: w}
	key := "mcp_servers." + name + "."
	ed.set(key+"command", e.Command)
	if len(e.Args) > 0 {
		ed.set(key+"args", e.Args)
	}
	ed.set(key+"url", e.URL)
	ed.set(key+"source", e.Source)
	setMap(ed, key+"env", e.Env)
	setMap(ed, key+"headers", e.Headers)
	if e.OAuth != nil {
		ed.set(key+"oauth", map[string]interface{}{})
	}
	if ed.err != nil {
		return ed.err
	}
	if err := keepBackup(path); err != nil {
		return err
	}
	return w.WriteConfig()
}

func setMap(ed *edit, key string, m map[string]string) {
	if len(m) == 0 {
		return
	}
	v := make(map[string]interface{}, len(m))
	for k, s := range m {
		v[k] = s
	}
	ed.set(key, v)
}
