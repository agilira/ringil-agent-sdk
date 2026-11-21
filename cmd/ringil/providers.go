// providers.go: which API the model speaks, and which key goes to it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"cmp"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

// Providers: OpenAI-compatible chat completions (the default), or
// Anthropic's Messages API.
const (
	providerOpenAI    = "openai"
	providerAnthropic = "anthropic"
	// anthropicKey holds the key of an Anthropic endpoint by default.
	anthropicKey = "ANTHROPIC_API_KEY"
)

var providers = map[string]bool{"": true, providerOpenAI: true, providerAnthropic: true}

func (f *config) checkProvider() error {
	if !providers[f.Provider] {
		return errors.New(ErrUsage, "provider is openai or anthropic")
	}
	return nil
}

// checkProvider: a known provider, and no embeddings asked of Anthropic.
func (p profile) checkProvider(name string) error {
	switch {
	case !providers[p.Provider]:
		return errors.New(ErrUsage, "profile "+name+": provider is openai or anthropic")
	case p.Provider == providerAnthropic && p.EmbedModel != "":
		return errors.New(ErrUsage, "profile "+name+": Anthropic has no embedding models (use embedder kind ollama)")
	}
	return nil
}

// mainProvider is the provider of the main model: --provider,
// RINGIL_PROVIDER, ringil.yaml, else openai.
func (c *cli) mainProvider(ctx *orpheus.Context, res secrets.Resolver) (string, error) {
	v, err := c.setting(ctx, res, "provider", "RINGIL_PROVIDER")
	if err != nil {
		return "", err
	}
	if !providers[v] {
		return "", errors.New(ErrUsage, "provider is openai or anthropic, not "+v)
	}
	return cmp.Or(v, providerOpenAI), nil
}

// providerKind is the provider of the run's model: the role's, else the
// main one.
func (c *cli) providerKind(ctx *orpheus.Context, res secrets.Resolver) (string, error) {
	if c.role != nil && c.role.Provider != "" {
		return c.role.Provider, nil
	}
	return c.mainProvider(ctx, res)
}

// baseFor is the endpoint of the run's model. RINGIL_BASE_URL is the
// OpenAI-compatible endpoint's: Anthropic's comes only from --base, or
// from ringil.yaml's base when the file says provider: anthropic too (or
// from the role), so that its key never goes to the other (21/11: the
// environment's, then the file's, base took it to the router).
func (c *cli) baseFor(ctx *orpheus.Context, res secrets.Resolver, kind string) (string, error) {
	own, err := c.ownEndpoint(ctx, res)
	switch {
	case err != nil:
		return "", err
	case own:
		return c.role.Base, nil
	case kind != providerAnthropic:
		return c.roleSetting(ctx, res, "base", "RINGIL_BASE_URL")
	}
	if ctx.GlobalFlagChanged("base") {
		return ctx.GetGlobalFlagString("base"), nil
	}
	if f := c.conf(); f.Provider == providerAnthropic {
		return f.Base, nil
	}
	return "", nil
}

// keepImages is how many of the latest messages send their images: all
// to Claude; to an OpenAI-compatible server only the latest, as local ones
// (llama.cpp, some vLLM builds) take one image per request.
func (c *cli) keepImages(ctx *orpheus.Context) int {
	res, err := c.resolver(ctx)
	if err != nil {
		return 1
	}
	if kind, err := c.providerKind(ctx, res); err == nil && kind == providerAnthropic {
		return 0
	}
	return 1
}

// ownEndpoint: the role talks to an endpoint of its own, its base or
// another provider than the main one; it sends only its own key there,
// and the main key never leaves for it.
func (c *cli) ownEndpoint(ctx *orpheus.Context, res secrets.Resolver) (bool, error) {
	if c.role == nil {
		return false, nil
	}
	if c.role.Base != "" {
		return true, nil
	}
	if c.role.Provider == "" {
		return false, nil
	}
	main, err := c.mainProvider(ctx, res)
	return c.role.Provider != main, err
}
