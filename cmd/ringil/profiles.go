// profiles.go: roles with their own model, endpoint and prompt
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
	"regexp"
	"sort"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/secrets"
)

// profile is a role: what it overrides of the command line's settings.
// Empty fields keep the flag or environment value.
type profile struct {
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
	Base        string `json:"base,omitempty"`
	// KeyEnv names the variable holding this role's API key. A role with
	// its own base sends only this key, none without it: the main key
	// never goes to another endpoint. It needs base: a role's key never
	// goes to the main endpoint either.
	KeyEnv string `json:"key_env,omitempty"`
	// EmbedModel is the embedding model at the role's base, for memory by
	// meaning. A role with its own base and none keeps memory by words:
	// memories never go to another endpoint.
	EmbedModel string `json:"embed_model,omitempty"`
	System     string `json:"system,omitempty"`
	MaxTurns   int    `json:"max_turns,omitempty"`
	// Images says the role's model reads images; for a role with its own
	// model only (else RINGIL_IMAGES decides).
	Images bool `json:"images,omitempty"`
	// Tools, when present, are the only tools the role has (by name; an
	// empty list, none): what a chat's members or the public may use.
	// Absent, every tool.
	Tools *[]string `json:"tools,omitempty"`
	// Memory is what the role remembers: "" the user's own memory,
	// "chat" a memory for each chat (its conversations share it, nobody
	// else sees it), "off" none.
	Memory string `json:"memory,omitempty"`
}

type profilesFile struct {
	Profiles map[string]profile `json:"profiles"`
}

var profileName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (c *cli) profilesPath(ctx *orpheus.Context) string {
	if p := ctx.GetGlobalFlagString("profiles-config"); p != "" {
		return p
	}
	base, err := c.configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "ringil", "profiles.json")
}

// loadProfiles reads the roles: a private file, like the other settings
// that decide where prompts go. A missing default file is no roles.
func (c *cli) loadProfiles(ctx *orpheus.Context) (map[string]profile, error) {
	path := c.profilesPath(ctx)
	if path == "" {
		return nil, nil
	}
	data, err := readPrivate(path)
	if err != nil {
		if ctx.GetGlobalFlagString("profiles-config") == "" && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errors.Wrap(err, ErrUsage, "cannot read the profiles in "+path)
	}
	var f profilesFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil || dec.More() {
		return nil, errors.New(ErrUsage, path+" is not one valid profiles object")
	}
	for name, p := range f.Profiles {
		if err := p.check(name); err != nil {
			return nil, err
		}
	}
	return f.Profiles, nil
}

func (p profile) check(name string) error {
	switch {
	case !profileName.MatchString(name) || name == "default":
		return errors.New(ErrUsage, "profile names are 1-64 letters, digits, - _, and not \"default\": "+name)
	case p.KeyEnv != "" && secrets.ValidateName(p.KeyEnv) != nil:
		return errors.New(ErrUsage, "profile "+name+": key_env is not a valid variable name")
	case p.Base == "" && p.KeyEnv+p.EmbedModel != "":
		return errors.New(ErrUsage, "profile "+name+": key_env and embed_model need the role's own base")
	}
	return p.checkLimits(name)
}

func (p profile) checkLimits(name string) error {
	switch {
	case p.MaxTurns < 0 || len(p.System) > 64<<10:
		return errors.New(ErrUsage, "profile "+name+": max_turns cannot be negative, the system prompt is at most 64 KiB")
	case p.Memory != "" && p.Memory != "chat" && p.Memory != "off":
		return errors.New(ErrUsage, "profile "+name+": memory is \"chat\" or \"off\", or left out for the user's own")
	}
	return nil
}

// restricted: the role lists its tools, as a chat's members or the
// public need. It has no workers, skills, notebooks or project rules of
// the user, and none of the user's artifacts.
func (c *cli) restricted() bool {
	return c.role != nil && c.role.Tools != nil
}

// ownersOnly are tools a restricted role may not list: they reach what is
// the user's.
var ownersOnly = map[string]bool{"list_artifacts": true, "read_artifact": true, "publish_artifact": true}

// onlyTools keeps the role's tools, when it lists them; a name that is
// no tool is an error, not a silent gap.
func (c *cli) onlyTools(tools agent.Registry) (agent.Registry, error) {
	if !c.restricted() {
		return tools, nil
	}
	if tools == nil {
		tools, _ = agent.NewRegistry()
	}
	for _, name := range *c.role.Tools {
		switch {
		case ownersOnly[name]:
			return nil, errors.New(ErrUsage, "a profile that lists its tools cannot have "+name+": it reaches the user's own artifacts")
		case !tools.Has(name):
			return nil, errors.New(ErrUsage, "the profile lists a tool it does not have: "+name)
		}
	}
	return tools.Select(*c.role.Tools...)
}

// checkChatProfile accepts a profile for a chat's members or the public:
// one of the profiles file, listing its tools, with memory per chat or
// none: never the user's own agent, tools or memory.
func checkChatProfile(roles map[string]profile, name string) error {
	p, ok := roles[name]
	switch {
	case name == "":
		return nil
	case !ok:
		return errors.New(ErrUsage, "no profile "+name+" in the profiles file (and not default: it is the owner's)")
	case p.Tools == nil:
		return errors.New(ErrUsage, "profile "+name+" serves chats: list its tools (\"tools\": [] for none)")
	case p.Memory != "chat" && p.Memory != "off":
		return errors.New(ErrUsage, "profile "+name+" serves chats: its memory must be \"chat\" or \"off\"")
	}
	return nil
}

// chooseProfile applies --profile: the named role's settings. A role
// already set (a profile served) stays; under serve --profile only names
// the profile --a2a offers.
func (c *cli) chooseProfile(ctx *orpheus.Context) error {
	name := ctx.GetGlobalFlagString("profile")
	if c.role != nil || c.server != nil || name == "" || name == "default" {
		return nil
	}
	all, err := c.loadProfiles(ctx)
	if err != nil {
		return err
	}
	p, ok := all[name]
	if !ok {
		return errors.New(ErrUsage, "no profile "+name+" in "+c.profilesPath(ctx))
	}
	c.role = &p
	return nil
}

// names lists the profiles, sorted.
func names(all map[string]profile) []string {
	out := make([]string, 0, len(all))
	for n := range all {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Overrides of the role, when there is one.

func (c *cli) roleSetting(ctx *orpheus.Context, res secrets.Resolver, flag, env string) (string, error) {
	if c.role != nil {
		switch {
		case flag == "base" && c.role.Base != "":
			return c.role.Base, nil
		case flag == "model" && c.role.Model != "":
			return c.role.Model, nil
		}
	}
	return c.setting(ctx, res, flag, env)
}

// keyEnv names the key's variable; "" sends none. A role with its own
// base uses only its own key.
func (c *cli) keyEnv(ctx *orpheus.Context) string {
	if c.role != nil && c.role.Base != "" {
		return c.role.KeyEnv
	}
	return ctx.GetGlobalFlagString("key-env")
}

// embedModel is the embedding model to use: the role's at the role's own
// base, else the main setting.
func (c *cli) embedModel(ctx *orpheus.Context, res secrets.Resolver) (string, error) {
	if c.role != nil && c.role.Base != "" {
		return c.role.EmbedModel, nil
	}
	return c.setting(ctx, res, "embed-model", "RINGIL_EMBED_MODEL")
}

func (c *cli) system(ctx *orpheus.Context) string {
	if c.role != nil && c.role.System != "" {
		return c.role.System
	}
	return ctx.GetGlobalFlagString("system")
}

func (c *cli) maxTurns(ctx *orpheus.Context) int {
	if c.role != nil && c.role.MaxTurns > 0 {
		return c.role.MaxTurns
	}
	return ctx.GetGlobalFlagInt("max-turns")
}

// images says whether the model reads images: the role's, for a role
// with its own model, else RINGIL_IMAGES=1 (environment or --env-file).
func (c *cli) images(ctx context.Context, res secrets.Resolver) bool {
	if c.role != nil && c.role.Model != "" {
		return c.role.Images
	}
	v, _, err := secrets.Lookup(ctx, res, "RINGIL_IMAGES")
	return err == nil && v == "1"
}
