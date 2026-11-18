// a2a.go: remote agents from the user's agents.json
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/a2a"
)

// agentsFile: {"agents": {"warehouse": {"url": "https://...", "headers": {...}}}}.
type agentsFile struct {
	Agents map[string]remoteEntry `json:"agents"`
}

type remoteEntry struct {
	URL              string            `json:"url"`
	Headers          map[string]string `json:"headers,omitempty"` // ${NAME} resolved per request
	AllowOtherOrigin bool              `json:"allowOtherOrigin,omitempty"`
	Disabled         bool              `json:"disabled,omitempty"`
}

func (c *cli) agentsPath(ctx *orpheus.Context) string {
	if p := ctx.GetGlobalFlagString("agents-config"); p != "" {
		return p
	}
	base, err := c.configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "ringil", "agents.json")
}

func loadAgents(path string, explicit bool) (map[string]remoteEntry, error) {
	data, err := readPrivate(path)
	if os.IsNotExist(err) && !explicit {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "cannot read the agents configuration "+path)
	}
	// Our own file: a mistyped key ("disable") is reported, not ignored.
	var f agentsFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, errors.Wrap(err, ErrUsage, "the agents configuration is not valid JSON of the expected shape")
	}
	return f.Agents, nil
}

// connectAgents reads every remote agent's card; one that fails is
// reported and left out.
func (c *cli) connectAgents(ctx *orpheus.Context) ([]agent.Tool, error) {
	path := c.agentsPath(ctx)
	if path == "" {
		return nil, nil
	}
	entries, err := loadAgents(path, ctx.GetGlobalFlagString("agents-config") != "")
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var tools []agent.Tool
	for _, name := range names {
		e := entries[name]
		if e.Disabled {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx.Context(), 30*time.Second)
		remote, err := a2a.Connect(cctx, a2a.Remote{Name: name, URL: e.URL, AllowOtherOrigin: e.AllowOtherOrigin, Headers: headerFunc(e.Headers, res)})
		cancel()
		if err != nil {
			c.note("remote agent " + name + " left out: " + err.Error())
			continue
		}
		c.closers = append(c.closers, remote.Close)
		tools = append(tools, remote.Tool())
	}
	return tools, nil
}
