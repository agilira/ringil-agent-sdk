// models.go: the run's models, in order: the first answers, the others
// when it does not
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fallback"
	"github.com/agilira/ringil-agent-sdk/secrets"
)

// modelRef is one entry of models: "provider:model", the provider by name
// (a built-in one or one of providers:), the model as that provider names
// it ("ollama:qwen3:8b", "openrouter:anthropic/claude-sonnet-5.5"). The
// list is what the app shows: a connection and a model a row, the first
// the main one, the others answering in order when it does not.
type modelRef struct{ provider, model string }

func (m modelRef) String() string { return m.provider + ":" + m.model }

func parseModel(entry string) (modelRef, error) {
	prov, model, ok := strings.Cut(strings.TrimSpace(entry), ":")
	if !ok || prov == "" || strings.TrimSpace(model) == "" {
		return modelRef{}, errors.New(ErrUsage, "models: "+entry+" is not provider:model (anthropic:claude-opus-5-5)")
	}
	return modelRef{prov, strings.TrimSpace(model)}, nil
}

func parseModels(entries []string) ([]modelRef, error) {
	out := make([]modelRef, 0, len(entries))
	for _, e := range entries {
		m, err := parseModel(e)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// checkModels: every entry is provider:model with a provider ringil knows.
func (f *config) checkModels(where string, entries []string) error {
	list, err := parseModels(entries)
	if err != nil {
		return errors.New(ErrUsage, where+err.Error())
	}
	for _, m := range list {
		if _, err := f.endpointNamed(m.provider); err != nil {
			return errors.New(ErrUsage, where+"models "+m.String()+": "+err.Error())
		}
	}
	return nil
}

func (f *config) checkAllModels() error {
	if err := f.checkModels("", f.Models); err != nil {
		return err
	}
	for _, name := range names(f.Profiles) {
		if err := f.checkModels("profile "+name+": ", f.Profiles[name].Models); err != nil {
			return err
		}
	}
	return nil
}

// mainProviderName is the main model's provider, whatever the role.
func (c *cli) mainProviderName(ctx *orpheus.Context, res secrets.Resolver) (string, error) {
	list, err := c.mainModels(ctx, res)
	if err != nil {
		return "", err
	}
	return list[0].provider, nil
}

// first is the main model of a list, if any.
func first(entries []string) (modelRef, bool) {
	if len(entries) == 0 {
		return modelRef{}, false
	}
	m, err := parseModel(entries[0])
	return m, err == nil
}

// runModels is the run's list. A role with its own models uses them
// alone: its conversations go to no model it does not name (audit 25/11:
// a profile kept on Ollama reached the flag's provider). Else --models or
// ringil.yaml's models, with --provider and --model (or RINGIL_PROVIDER,
// RINGIL_MODEL) choosing the main one: it goes first, the list after it.
func (c *cli) runModels(ctx *orpheus.Context, res secrets.Resolver) ([]modelRef, error) {
	if c.role != nil && len(c.role.Models) > 0 {
		return parseModels(c.role.Models)
	}
	return c.mainModels(ctx, res)
}

// mainModels is the list of the main role, whatever the run's role.
func (c *cli) mainModels(ctx *orpheus.Context, res secrets.Resolver) ([]modelRef, error) {
	entries := c.conf().Models
	if ctx.GlobalFlagChanged("models") {
		entries = splitList(ctx.GetGlobalFlagString("models"))
	}
	list, err := parseModels(entries)
	if err != nil {
		return nil, err
	}
	main, err := c.mainModel(ctx, res, list)
	if err != nil {
		return nil, err
	}
	out := []modelRef{main}
	for _, m := range list {
		if m != main {
			out = append(out, m)
		}
	}
	return out, nil
}

// mainModel is the first of list, or the one --provider and --model name:
// a model alone keeps the list's provider, a provider alone the list's
// model only if it is that model's provider.
func (c *cli) mainModel(ctx *orpheus.Context, res secrets.Resolver, list []modelRef) (modelRef, error) {
	prov, err := c.providerSetting(ctx, res, "provider", "RINGIL_PROVIDER", "")
	if err != nil {
		return modelRef{}, err
	}
	model, err := c.setting(ctx, res, "model", "RINGIL_MODEL")
	if err != nil {
		return modelRef{}, err
	}
	var top modelRef
	if len(list) > 0 {
		top = list[0]
	}
	return choose(prov, model, top)
}

// choose is the main model of prov and model given (either may be
// empty) over the list's first, top.
func choose(prov, model string, top modelRef) (modelRef, error) {
	if prov == "" {
		prov = top.provider
	}
	if model == "" && prov == top.provider {
		model = top.model
	}
	switch {
	case prov == "" && model == "":
		return modelRef{}, errors.New(ErrUsage, "no model: set models in ringil.yaml (ringil setup), or --provider and --model")
	case prov == "":
		return modelRef{}, errors.New(ErrUsage, "no provider for "+model+": set models in ringil.yaml (ringil setup), or --provider")
	case model == "":
		return modelRef{}, errors.New(ErrUsage, "set --model, or models in ringil.yaml (ringil setup offers the list)")
	}
	return modelRef{prov, model}, nil
}

// chain makes the providers of list: the first is the main one, the others
// answer when it does not. Only models ringil can reach count: one whose
// provider has no key, or that cannot be made (ChatGPT not signed in), is
// left out and said; with one left, it is used alone and nothing changes.
func (c *cli) chain(ctx *orpheus.Context, res secrets.Resolver, list []modelRef) (agent.Provider, error) {
	ep, err := c.conf().endpointNamed(list[0].provider)
	if err != nil {
		return nil, err
	}
	main, err := c.newModel(ctx, res, ep, list[0].model)
	if err != nil {
		return nil, err
	}
	providers := []agent.Provider{main}
	for _, m := range list[1:] {
		ep, err := c.conf().endpointNamed(m.provider)
		if err != nil {
			return nil, err
		}
		if !c.reachable(ctx, res, ep) {
			_ = c.line("  * " + m.String() + ": no key for " + m.provider + ", left out")
			continue
		}
		p, err := c.newModel(ctx, res, ep, m.model)
		if err != nil {
			_ = c.line("  * " + m.String() + ": " + describe(err) + ", left out")
			continue
		}
		providers = append(providers, p)
	}
	return fallback.New(providers, fallback.Config{OnSwitch: c.switched})
}

// reachable: ep needs no key, or its key is there.
func (c *cli) reachable(ctx *orpheus.Context, res secrets.Resolver, ep endpoint) bool {
	return c.reachableIn(ctx.Context(), res, ep)
}

// switched tells the terminal which model answers now.
func (c *cli) switched(s fallback.Switch) {
	if s.Err != nil {
		_ = c.line("  * " + s.From + " did not answer (" + describe(s.Err) + "): trying " + s.To)
		return
	}
	_ = c.line("  * " + s.To + " answers again")
}
