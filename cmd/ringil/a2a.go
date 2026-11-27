// a2a.go: remote agents, from the agents of ringil.yaml
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"sort"
	"time"

	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/a2a"
)

// agentsFile: {"agents": {"warehouse": {"url": "https://...", "headers": {...}}}}.
type remoteEntry struct {
	URL              string            `argus:"url"`
	Headers          map[string]string `argus:"headers"` // ${NAME} resolved per request
	AllowOtherOrigin bool              `argus:"allow_other_origin"`
	Disabled         bool              `argus:"disabled"`
}

// connectAgents reads every remote agent's card; one that fails is
// reported and left out.
func (c *cli) connectAgents(ctx *orpheus.Context) ([]agent.Tool, error) {
	entries := c.conf().Agents
	if len(entries) == 0 {
		return nil, nil
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
