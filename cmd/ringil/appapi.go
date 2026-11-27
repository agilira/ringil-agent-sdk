// appapi.go: the settings an app changes through serve
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/agilira/argus"
	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus"
	agent "github.com/agilira/ringil-agent-sdk"

	"github.com/agilira/ringil-agent-sdk/mcp"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/skills"
)

// The settings API, mounted on serve behind its guard (the token, or the
// app's session):
//
//	GET    /v1/settings                          models, connections, limits, keys set, MCP servers, skills, profiles
//	PUT    /v1/settings/models                   {"models":["provider:model",...]} the first answers, the others stand in
//	PUT    /v1/settings/limits                   {"max_cost","workflow_cost"} US dollars, 0 none
//	PUT    /v1/settings/voice                    {"transcribe":{"provider","model"},"speech":{"provider","model","voice"}}
//	PUT    /v1/settings/providers/{name}         {"kind"?,"base"?,"key_env"?} a connection of one's own, or a built-in's address
//	DELETE /v1/settings/providers/{name}         remove it
//	GET    /v1/settings/providers/{name}/models  the models the provider lists
//	PUT    /v1/settings/secrets/{name}           {"value"} kept in the secrets store; never read back
//	DELETE /v1/settings/secrets/{name}           remove it
//	POST   /v1/settings/mcp_servers              {"name","command"|"url","env"?,"headers"?} add and try one
//	DELETE /v1/settings/mcp_servers/{name}       remove it
//	POST   /v1/settings/skills                   {"url"} install one from GitHub
//	DELETE /v1/settings/skills/{name}            remove it
//
// Every change is checked as the whole file would be before it is written
// to ringil.yaml (a backup first, the rest of the file untouched); serve
// takes it at the next run.
func (c *cli) mountSettings(ctx *orpheus.Context, srv *serve.Server) {
	routes := map[string]http.HandlerFunc{
		"GET /v1/settings":                         c.getSettings(ctx),
		"PUT /v1/settings/models":                  c.putModels,
		"PUT /v1/settings/limits":                  c.putLimits,
		"PUT /v1/settings/voice":                   c.putVoice,
		"PUT /v1/settings/providers/{name}":        c.putProvider,
		"DELETE /v1/settings/providers/{name}":     c.deleteProvider,
		"GET /v1/settings/providers/{name}/models": c.providerModels(ctx),
		"PUT /v1/settings/secrets/{name}":          c.putSecret(ctx),
		"DELETE /v1/settings/secrets/{name}":       c.deleteSecret(ctx),
		"POST /v1/settings/mcp_servers":            c.postMCP(ctx),
		"DELETE /v1/settings/mcp_servers/{name}":   c.deleteMCP,
		"POST /v1/settings/skills":                 c.postSkill,
		"DELETE /v1/settings/skills/{name}":        c.deleteSkill,
	}
	for pattern, h := range routes {
		srv.Mount(pattern, h)
	}
}

// Settings is what GET /v1/settings answers: never a secret's value.
type Settings struct {
	Models       []string       `json:"models"`
	Providers    []ProviderInfo `json:"providers"`
	MaxCost      float64        `json:"max_cost,omitzero"`
	WorkflowCost float64        `json:"workflow_cost,omitzero"`
	SecretsStore string         `json:"secrets_store,omitzero"`
	MCPServers   []MCPInfo      `json:"mcp_servers"`
	Skills       []skills.Info  `json:"skills"`
	Profiles     []string       `json:"profiles"`
	Transcribe   VoiceModel     `json:"transcribe"`
	Speech       VoiceModel     `json:"speech"`
}

// VoiceModel is the model of one half of the voice; no model, it is off.
type VoiceModel struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Voice    string `json:"voice,omitempty"` // speech only
}

// ProviderInfo is a connection: where, and whether its key is set.
type ProviderInfo struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Base    string `json:"base,omitzero"`
	KeyEnv  string `json:"key_env,omitzero"`
	Builtin bool   `json:"builtin,omitzero"`
	// KeySet: the key is there (in the store, or the environment without
	// one); ChatGPT and servers with no key are always ready.
	KeySet bool `json:"key_set"`
}

// MCPInfo is an MCP server of ringil.yaml.
type MCPInfo struct {
	Name    string   `json:"name"`
	Command []string `json:"command,omitzero"`
	URL     string   `json:"url,omitzero"`
}

func (c *cli) getSettings(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := c.conf()
		out := Settings{Models: append([]string{}, f.Models...), MaxCost: f.MaxCost, WorkflowCost: f.WorkflowCost,
			SecretsStore: f.Secrets.Store, Profiles: names(f.Profiles), MCPServers: []MCPInfo{}, Skills: []skills.Info{},
			Transcribe: VoiceModel{Provider: f.Transcribe.Provider, Model: f.Transcribe.Model},
			Speech:     VoiceModel{Provider: f.Speech.Provider, Model: f.Speech.Model, Voice: f.Speech.Voice}}
		res, _ := c.resolver(octx)
		for _, name := range providerNames(f) {
			ep, err := f.endpointNamed(name)
			if err != nil {
				continue
			}
			_, builtin := builtinProviders[name]
			out.Providers = append(out.Providers, ProviderInfo{Name: name, Kind: ep.kind, Base: ep.base, KeyEnv: ep.keyName,
				Builtin: builtin, KeySet: res != nil && c.reachableIn(r.Context(), res, ep)})
		}
		for _, name := range sortedKeys(f.MCPServers) {
			e := f.MCPServers[name]
			out.MCPServers = append(out.MCPServers, MCPInfo{Name: name, Command: append([]string{e.Command}, e.Args...), URL: e.URL})
			if e.Command == "" {
				out.MCPServers[len(out.MCPServers)-1].Command = nil
			}
		}
		if cat, err := c.skillCatalog(octx); err == nil && cat != nil {
			out.Skills = cat.List()
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// providerNames are the built-in providers and the file's own, sorted.
func providerNames(f *config) []string {
	all := slices.Collect(maps.Keys(builtinProviders))
	for name := range f.Providers {
		if !slices.Contains(all, name) {
			all = append(all, name)
		}
	}
	slices.Sort(all)
	return all
}

// reachableIn: ep needs no key, or its key is there.
func (c *cli) reachableIn(ctx context.Context, res secrets.Resolver, ep endpoint) bool {
	if ep.kind == providerChatGPT || ep.keyName == "" {
		return true
	}
	key, err := keyFunc(res, ep.keyName)(ctx)
	return err == nil && key != ""
}

type modelsIn struct {
	Models []string `json:"models"`
}

func (c *cli) putModels(w http.ResponseWriter, r *http.Request) {
	var in modelsIn
	if !decodeJSON(w, r, &in) {
		return
	}
	c.change(w, func(next *config) *refusal { next.Models = in.Models; return nil },
		func(ed *edit) { ed.set("models", in.Models) })
}

type limitsIn struct {
	MaxCost      float64 `json:"max_cost"`
	WorkflowCost float64 `json:"workflow_cost"`
}

func (c *cli) putLimits(w http.ResponseWriter, r *http.Request) {
	var in limitsIn
	if !decodeJSON(w, r, &in) {
		return
	}
	c.change(w, func(next *config) *refusal {
		next.MaxCost, next.WorkflowCost = in.MaxCost, in.WorkflowCost
		return nil
	}, func(ed *edit) {
		ed.set("max_cost", zeroAsNone(in.MaxCost))
		ed.set("workflow_cost", zeroAsNone(in.WorkflowCost))
	})
}

// zeroAsNone: a limit of 0 is no limit, so its key goes.
func zeroAsNone(v float64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

type voiceIn struct {
	Transcribe VoiceModel `json:"transcribe"`
	Speech     VoiceModel `json:"speech"`
}

func (c *cli) putVoice(w http.ResponseWriter, r *http.Request) {
	var in voiceIn
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Transcribe.Voice != "" {
		fail(w, http.StatusBadRequest, "SETTINGS_INPUT", "a voice is for speech, not transcription")
		return
	}
	c.change(w, func(next *config) *refusal {
		next.Transcribe = audioConfig{Provider: in.Transcribe.Provider, Model: in.Transcribe.Model}
		next.Speech = audioConfig{Provider: in.Speech.Provider, Model: in.Speech.Model, Voice: in.Speech.Voice}
		return nil
	}, func(ed *edit) {
		ed.set("transcribe", nil)
		ed.set("speech", nil)
		ed.merge("transcribe", "provider", in.Transcribe.Provider, "model", in.Transcribe.Model)
		ed.merge("speech", "provider", in.Speech.Provider, "model", in.Speech.Model, "voice", in.Speech.Voice)
	})
}

type providerIn struct {
	Kind   string `json:"kind,omitempty"`
	Base   string `json:"base,omitempty"`
	KeyEnv string `json:"key_env,omitempty"`
}

func (c *cli) putProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in providerIn
	if !decodeJSON(w, r, &in) {
		return
	}
	c.change(w, func(next *config) *refusal { next.Providers[name] = providerEntry(in); return nil }, func(ed *edit) {
		ed.set("providers."+name, map[string]interface{}{})
		ed.set("providers."+name+".kind", in.Kind)
		ed.set("providers."+name+".base", in.Base)
		ed.set("providers."+name+".key_env", in.KeyEnv)
	})
}

func (c *cli) deleteProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c.change(w, func(next *config) *refusal {
		if _, ok := next.Providers[name]; !ok {
			return notFound("no provider " + name + " of your own in ringil.yaml")
		}
		delete(next.Providers, name)
		return nil
	}, func(ed *edit) { ed.set("providers."+name, nil) })
}

func (c *cli) providerModels(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ep, err := c.conf().endpointNamed(r.PathValue("name"))
		if err != nil {
			fail(w, http.StatusNotFound, "SETTINGS_NOT_FOUND", describe(err))
			return
		}
		res, err := c.resolver(octx)
		if err != nil {
			fail(w, http.StatusInternalServerError, "SETTINGS_SECRETS", describe(err))
			return
		}
		s := &setup{c: c, ctx: r.Context(), endpoint: ep}
		if ep.keyName != "" {
			s.key, _ = keyFunc(res, ep.keyName)(r.Context())
		}
		ids, err := s.models()
		if err != nil {
			fail(w, http.StatusBadGateway, "SETTINGS_PROVIDER", describe(err))
			return
		}
		writeJSON(w, http.StatusOK, map[string][]string{"models": chatModels(ids)})
	}
}

type secretIn struct {
	Value string `json:"value"`
}

func (c *cli) putSecret(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in secretIn
		if !decodeJSON(w, r, &in) {
			return
		}
		st, name, ok := c.secretStore(w, r, octx)
		if !ok {
			return
		}
		if in.Value == "" || len(in.Value) > 64<<10 || strings.ContainsRune(in.Value, 0) {
			fail(w, http.StatusBadRequest, "SETTINGS_INPUT", "a secret is 1 byte to 64 KiB of text")
			return
		}
		if err := st.Set(r.Context(), name, in.Value); err != nil {
			fail(w, http.StatusInternalServerError, "SETTINGS_SECRETS", describe(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (c *cli) deleteSecret(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, name, ok := c.secretStore(w, r, octx)
		if !ok {
			return
		}
		if err := st.Delete(r.Context(), name); err != nil {
			fail(w, http.StatusInternalServerError, "SETTINGS_SECRETS", describe(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// secretStore is the store a secret goes to, and its name, checked.
func (c *cli) secretStore(w http.ResponseWriter, r *http.Request, octx *orpheus.Context) (secrets.Store, string, bool) {
	name := r.PathValue("name")
	if err := secrets.ValidateName(name); err != nil {
		fail(w, http.StatusBadRequest, "SETTINGS_INPUT", describe(err))
		return nil, "", false
	}
	base, err := c.baseResolver(octx)
	if err != nil {
		fail(w, http.StatusInternalServerError, "SETTINGS_SECRETS", describe(err))
		return nil, "", false
	}
	c.keepMu.Lock() // one store, even for two first keys at once
	st, err := c.appStore(r.Context(), base)
	c.keepMu.Unlock()
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, "SETTINGS_SECRETS", "no secrets store could be opened or set up: "+describe(err))
		return nil, "", false
	case st == nil:
		fail(w, http.StatusConflict, "SETTINGS_NO_STORE", "serve runs without a configuration directory: no secrets store can be set up")
		return nil, "", false
	}
	return st, name, true
}

func (c *cli) postMCP(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var e mcp.Entry
		if !decodeJSON(w, r, &e) {
			return
		}
		if err := e.Check(); err != nil {
			fail(w, http.StatusBadRequest, "SETTINGS_INPUT", describe(err))
			return
		}
		added, err := c.addMCP(r.Context(), e, func() (secrets.Resolver, error) { return c.resolver(octx) })
		switch {
		case errors.HasCode(err, mcp.ErrConnect), errors.HasCode(err, errMCPNextSession): // saved, but not started now
			c.settings.signal()
			writeJSON(w, http.StatusCreated, map[string]any{"tools": []string{}, "missing": []string{}, "warning": agent.ModelMessage(err)})
		case errors.HasCode(err, errMCPExists):
			fail(w, http.StatusConflict, "SETTINGS_EXISTS", agent.ModelMessage(err))
		case err != nil:
			fail(w, http.StatusBadRequest, "SETTINGS_MCP", describe(err))
		default:
			c.settings.signal()
			writeJSON(w, http.StatusCreated, map[string]any{"tools": nonNil(added.Tools), "missing": nonNil(added.Missing)})
		}
	}
}

func (c *cli) deleteMCP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c.change(w, func(next *config) *refusal {
		if _, ok := next.MCPServers[name]; !ok {
			return notFound("no MCP server " + name)
		}
		delete(next.MCPServers, name)
		return nil
	}, func(ed *edit) { ed.set("mcp_servers."+name, nil) })
}

type skillIn struct {
	URL string `json:"url"`
}

func (c *cli) postSkill(w http.ResponseWriter, r *http.Request) {
	var in skillIn
	if !decodeJSON(w, r, &in) {
		return
	}
	dir, err := c.skillsDir()
	if err != nil {
		fail(w, http.StatusInternalServerError, "SETTINGS_SKILLS", describe(err))
		return
	}
	inst, err := skills.InstallGitHub(r.Context(), in.URL, dir, skills.GitHub{})
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTINGS_SKILLS", describe(err))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": inst.Name, "description": inst.Description, "source": inst.Source})
}

func (c *cli) deleteSkill(w http.ResponseWriter, r *http.Request) {
	dir, err := c.skillsDir()
	if err == nil {
		err = skills.Remove(dir, r.PathValue("name"))
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTINGS_SKILLS", describe(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// change writes a change to ringil.yaml when next, the whole file with
// it, passes every check; the rest of the file stays as it is.
// refusal is a change refused before anything is written.
type refusal struct {
	status     int
	code, what string
}

func notFound(what string) *refusal {
	return &refusal{http.StatusNotFound, "SETTINGS_NOT_FOUND", what}
}

// change makes one change to ringil.yaml, one at a time, on the file as
// it is on disk (a change by hand, or by the request before, is counted;
// a file that is not valid is said, not changed on top): mutate checks and
// makes the change on a copy, which must pass every check a revision
// passes; then apply writes it, and the watcher is woken.
func (c *cli) change(w http.ResponseWriter, mutate func(next *config) *refusal, apply func(*edit)) {
	c.keepMu.Lock()
	defer c.keepMu.Unlock()
	path := ""
	if c.settings != nil {
		path = c.settings.path
	}
	if path == "" {
		fail(w, http.StatusConflict, "SETTINGS_NO_FILE", "serve runs without a configuration directory: there is no ringil.yaml to change")
		return
	}
	cur, err := c.settings.onDisk(c)
	if err != nil {
		fail(w, http.StatusConflict, "SETTINGS_FILE", "ringil.yaml on disk is not valid, fix it first: "+describe(err))
		return
	}
	next := cur.clone()
	if r := mutate(next); r != nil {
		fail(w, r.status, r.code, r.what)
		return
	}
	err = next.Validate()
	if err == nil {
		err = c.settings.acceptable(next)
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTINGS_INPUT", describe(err))
		return
	}
	if err := writeSettings(path, apply); err != nil {
		fail(w, http.StatusInternalServerError, "SETTINGS_WRITE", describe(err))
		return
	}
	c.settings.signal()
	w.WriteHeader(http.StatusNoContent)
}

// writeSettings changes the file through argus's writer, after a backup.
// The caller holds keepMu.
func writeSettings(path string, apply func(*edit)) error {
	wr, err := argus.NewConfigWriter(path, argus.FormatYAML, nil)
	if err != nil {
		return errors.Wrap(err, ErrUsage, path+" cannot be edited")
	}
	ed := &edit{w: wr}
	apply(ed)
	if ed.err != nil {
		return ed.err
	}
	if err := keepBackup(path); err != nil {
		return err
	}
	return wr.WriteConfig()
}

// clone is f with its own maps, for a change checked before it is written.
func (f *config) clone() *config {
	next := *f
	next.Models = slices.Clone(f.Models)
	next.Providers = maps.Clone(f.Providers)
	if next.Providers == nil {
		next.Providers = map[string]providerEntry{}
	}
	next.MCPServers = maps.Clone(f.MCPServers)
	next.Profiles = maps.Clone(f.Profiles)
	return &next
}

// decodeJSON reads one JSON object of known fields, at most 64 KiB.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeJSONLimit(w, r, v, 64<<10)
}

// decodeJSONLimit is decodeJSON for a body of at most limit bytes.
func decodeJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		fail(w, http.StatusBadRequest, "SETTINGS_INPUT", "the body is not one JSON object of the expected fields")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, errCode, message string) {
	writeJSON(w, code, map[string]map[string]string{"error": {"code": errCode, "message": message}})
}

// nonNil is list, [] for none: an app reads a list, not null.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
