// providers.go: the model's provider, by name, and the key that goes to it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"cmp"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

// The APIs a provider speaks: OpenAI-compatible chat completions,
// Anthropic's Messages API, or ChatGPT's with a subscription.
const (
	providerOpenAI    = "openai"
	providerAnthropic = "anthropic"
)

// providerEntry is a provider of ringil.yaml's providers section: an
// endpoint, the API it speaks and the secret holding its key. Its key goes
// to its base and nowhere else; a role or an audio model on another
// provider sends that provider's key.
type providerEntry struct {
	// Kind is the API: openai (OpenAI-compatible, the default) or
	// anthropic. A built-in provider keeps its own.
	Kind string `argus:"kind"`
	// Base is the endpoint; for an OpenAI-compatible one, what comes
	// before /chat/completions.
	Base string `argus:"base"`
	// KeyEnv names the secret holding the key; "" sends none.
	KeyEnv string `argus:"key_env"`
}

// endpoint is a provider resolved: where requests go, and with which key.
type endpoint struct {
	name    string
	kind    string
	base    string
	keyName string // "" for none
}

// builtinProviders are ready without a providers section: provider: deepseek
// and the key in DEEPSEEK_API_KEY is all they need. An entry of the same
// name changes their base or key_env (a regional endpoint, another
// secret). Addresses checked on each vendor's documentation, 23/11.
var builtinProviders = map[string]providerEntry{
	"anthropic":  {Kind: providerAnthropic, KeyEnv: "ANTHROPIC_API_KEY"},
	"chatgpt":    {Kind: providerChatGPT},
	"openai":     {Kind: providerOpenAI, Base: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY"},
	"google":     {Kind: providerOpenAI, Base: "https://generativelanguage.googleapis.com/v1beta/openai", KeyEnv: "GEMINI_API_KEY"},
	"openrouter": {Kind: providerOpenAI, Base: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY"},
	"nvidia":     {Kind: providerOpenAI, Base: "https://integrate.api.nvidia.com/v1", KeyEnv: "NVIDIA_API_KEY"},
	"deepseek":   {Kind: providerOpenAI, Base: "https://api.deepseek.com", KeyEnv: "DEEPSEEK_API_KEY"},
	"requesty":   {Kind: providerOpenAI, Base: "https://router.requesty.ai/v1", KeyEnv: "REQUESTY_API_KEY"},
	"zai":        {Kind: providerOpenAI, Base: "https://api.z.ai/api/paas/v4", KeyEnv: "ZAI_API_KEY"},
	"moonshot":   {Kind: providerOpenAI, Base: "https://api.moonshot.ai/v1", KeyEnv: "MOONSHOT_API_KEY"},
	"ollama":     {Kind: providerOpenAI, Base: ollamaBase},
}

// builtinKey: name is the key of a built-in provider, or the generic
// RINGIL_API_KEY of earlier versions, which people still have set.
func builtinKey(name string) bool {
	if strings.EqualFold(name, "RINGIL_API_KEY") {
		return true
	}
	for _, p := range builtinProviders {
		if name != "" && strings.EqualFold(p.KeyEnv, name) { // Windows reads variables in any case
			return true
		}
	}
	return false
}

// ollamaBase is Ollama's local address.
const ollamaBase = "http://127.0.0.1:11434/v1"

var providerName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// errGone says where base and key_env went (23/11: they became named
// providers).
func errGone(where string) error {
	return errors.New(ErrUsage, where+"base and key_env are gone: name a provider (provider: NAME, a built-in one or one of the providers section, with its base and key_env); see docs/ringil.example.yaml")
}

// checkGone refuses the endpoint settings of before named providers,
// wherever they are, saying where they went.
func (f *config) checkGone() error {
	if f.OldBase+f.OldKeyEnv != "" {
		return errGone("")
	}
	for _, name := range names(f.Profiles) {
		if p := f.Profiles[name]; p.OldBase+p.OldKeyEnv != "" {
			return errGone("profiles." + name + ": ")
		}
	}
	for where, a := range map[string]audioConfig{"transcribe": f.Transcribe, "speech": f.Speech} {
		if a.OldBase+a.OldKeyEnv != "" {
			return errGone(where + ": ")
		}
	}
	return nil
}

// checkProviders: valid names, kinds, addresses and key names (a built-in
// keeps its kind, and ChatGPT's sign-in is not a key), and every provider
// named elsewhere in the file exists and speaks what is asked of it.
func (f *config) checkProviders() error {
	for _, name := range slices.Sorted(maps.Keys(f.Providers)) {
		if err := checkProvider(name, f.Providers[name]); err != nil {
			return err
		}
	}
	if f.Provider != "" {
		if _, err := f.endpointNamed(f.Provider); err != nil {
			return err
		}
	}
	for _, name := range names(f.Profiles) {
		if err := f.checkRoleProvider(name, f.Profiles[name]); err != nil {
			return err
		}
	}
	for _, a := range []audioConfig{f.Transcribe, f.Speech} {
		if err := f.checkOpenAIProvider("transcribe and speech", a.Provider); err != nil {
			return err
		}
	}
	return nil
}

func (f *config) checkRoleProvider(role string, p profile) error {
	if p.EmbedModel != "" {
		return f.checkOpenAIProvider("profile "+role+" (embed_model)", p.Provider)
	}
	if p.Provider == "" {
		return nil
	}
	_, err := f.endpointNamed(p.Provider)
	return err
}

// checkOpenAIProvider: name, when given, is an OpenAI-compatible provider
// (embeddings and audio are only theirs).
func (f *config) checkOpenAIProvider(where, name string) error {
	if name == "" {
		return nil
	}
	ep, err := f.endpointNamed(name)
	if err == nil && ep.kind != providerOpenAI {
		err = errors.New(ErrUsage, where+": "+name+" speaks "+ep.kind+", which has no embedding or audio models")
	}
	return err
}

func checkProvider(name string, p providerEntry) error {
	if !providerName.MatchString(name) {
		return errors.New(ErrUsage, "provider names are 1-64 lowercase letters, digits, - _: "+name)
	}
	if err := checkKind(name, p); err != nil {
		return err
	}
	if p.KeyEnv != "" && secrets.ValidateName(p.KeyEnv) != nil {
		return errors.New(ErrUsage, "providers."+name+": key_env is not a valid variable name")
	}
	if p.Base == "" {
		return nil
	}
	if _, err := parseBase(p.Base); err != nil {
		return errors.New(ErrUsage, "providers."+name+": base is "+err.Error())
	}
	return nil
}

// checkKind: a built-in keeps its kind (and ChatGPT signs in, with no
// key); one's own is openai or anthropic, and an OpenAI-compatible one
// has its base.
func checkKind(name string, p providerEntry) error {
	builtin, isBuiltin := builtinProviders[name]
	if isBuiltin {
		return checkBuiltin(name, p, builtin)
	}
	switch {
	case p.Kind != "" && p.Kind != providerOpenAI && p.Kind != providerAnthropic:
		return errors.New(ErrUsage, "providers."+name+": kind is openai (compatible) or anthropic")
	case p.Kind != providerAnthropic && p.Base == "":
		return errors.New(ErrUsage, "providers."+name+": an OpenAI-compatible provider needs its base")
	case builtinKey(p.KeyEnv):
		// That key is a vendor's: it would go to this endpoint (audit 23/11).
		return errors.New(ErrUsage, "providers."+name+": key_env "+p.KeyEnv+" is a built-in provider's key; give this one's its own name")
	}
	return nil
}

func checkBuiltin(name string, p, builtin providerEntry) error {
	switch {
	case p.Kind != "" && p.Kind != builtin.Kind:
		return errors.New(ErrUsage, "providers."+name+": a built-in provider keeps its kind ("+builtin.Kind+"); give yours another name")
	case builtin.Kind == providerChatGPT && p.KeyEnv+p.Base != "":
		// Its sign-in is a token for ChatGPT's own address alone (audit 23/11).
		return errors.New(ErrUsage, "providers.chatgpt: ChatGPT signs in (ringil login chatgpt), at its own address and with no key")
	case builtinKey(p.KeyEnv) && !strings.EqualFold(p.KeyEnv, builtin.KeyEnv):
		// Another vendor's key would go to this one (audit 23/11).
		return errors.New(ErrUsage, "providers."+name+": key_env "+p.KeyEnv+" is another provider's key")
	}
	return checkMoved(name, p, builtin)
}

// checkMoved: another address for a vendor's key is said in the file,
// never implied (audit 23/11).
func checkMoved(name string, p, builtin providerEntry) error {
	if p.Base != "" && p.Base != builtin.Base && builtin.KeyEnv != "" && p.KeyEnv == "" {
		return errors.New(ErrUsage, "providers."+name+": another base needs its key_env, even "+builtin.KeyEnv+": the key goes to that address")
	}
	return nil
}

// parseBase: an http or https address with a host, without credentials,
// a query or a fragment (whether plain http may carry a key is
// allow_http's call, at the request).
func parseBase(base string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New(ErrUsage, "an http:// or https:// address, without credentials or a query")
	}
	return u, nil
}

// endpointNamed resolves a provider: the file's entry over the built-in of
// the same name, field by field.
func (f *config) endpointNamed(name string) (endpoint, error) {
	builtin, isBuiltin := builtinProviders[name]
	own, isOwn := f.Providers[name]
	if !isBuiltin && !isOwn {
		return endpoint{}, errors.New(ErrUsage, "no provider "+name+": a built-in one ("+builtinNames()+") or one of ringil.yaml's providers")
	}
	keyName := own.KeyEnv
	if !isOwn || keyName == "" && isBuiltin {
		keyName = builtin.KeyEnv
	}
	return endpoint{
		name:    name,
		kind:    cmp.Or(builtin.Kind, own.Kind, providerOpenAI),
		base:    cmp.Or(own.Base, builtin.Base),
		keyName: keyName,
	}, nil
}

func builtinNames() string {
	names := slices.Sorted(maps.Keys(builtinProviders))
	out := names[0]
	for _, n := range names[1:] {
		out += ", " + n
	}
	return out
}

// mainProviderName is the main model's provider: --provider,
// RINGIL_PROVIDER, else ringil.yaml's provider.
func (c *cli) mainProviderName(ctx *orpheus.Context, res secrets.Resolver) (string, error) {
	name, err := c.providerSetting(ctx, res, "provider", "RINGIL_PROVIDER", c.conf().Provider)
	if err == nil && name == "" {
		err = errors.New(ErrUsage, "no provider: set provider in ringil.yaml (ringil setup), or --provider")
	}
	return name, err
}

// runEndpoint is the provider of the run's model: the role's, else the
// main one.
func (c *cli) runEndpoint(ctx *orpheus.Context, res secrets.Resolver) (endpoint, error) {
	if c.role != nil && c.role.Provider != "" {
		return c.conf().endpointNamed(c.role.Provider)
	}
	name, err := c.mainProviderName(ctx, res)
	if err != nil {
		return endpoint{}, err
	}
	return c.conf().endpointNamed(name)
}

// ownEndpoint: the role talks to another provider than the main one. Its
// embedding model is then the role's own (memories go to no other
// endpoint).
func (c *cli) ownEndpoint(ctx *orpheus.Context, res secrets.Resolver) (bool, error) {
	if c.role == nil || c.role.Provider == "" {
		return false, nil
	}
	main, err := c.providerSetting(ctx, res, "provider", "RINGIL_PROVIDER", c.conf().Provider)
	// No main provider, or one the environment names under a store (refused
	// where it is used): the role's is its own, and keeps its memories.
	return err != nil || c.role.Provider != main, nil
}

// keepImages is how many of the latest messages send their images: all
// to Claude; to an OpenAI-compatible server only the latest, as local ones
// (llama.cpp, some vLLM builds) take one image per request.
func (c *cli) keepImages(ctx *orpheus.Context) int {
	res, err := c.resolver(ctx)
	if err != nil {
		return 1
	}
	if ep, err := c.runEndpoint(ctx, res); err == nil && ep.kind == providerAnthropic {
		return 0
	}
	return 1
}
