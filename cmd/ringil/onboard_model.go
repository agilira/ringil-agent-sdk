// onboard_model.go: the model, its key, and a first try
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/argus"
	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
	"github.com/agilira/ringil-agent-sdk/provider/anthropic"
	"github.com/agilira/ringil-agent-sdk/provider/chatgpt"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
)

// providerChoice is one answer to "which models?": a provider, by name,
// resolved as the run resolves it.
type providerChoice struct {
	label string
	name  string // a built-in provider; "" asks for a server of one's own
	model string // suggested when the list cannot be read
}

// mainChoices are asked first; compatible opens the OpenAI-compatible
// services.
var mainChoices = []providerChoice{
	{label: "Anthropic (Claude)", name: "anthropic", model: "claude-sonnet-5-5"},
	{label: "ChatGPT, with your subscription (Plus, Pro, Business: no API key)", name: "chatgpt"},
	{label: "OpenAI", name: "openai"},
	{label: "Google (Gemini)", name: "google"},
	{label: "An OpenAI-compatible service (OpenRouter, NVIDIA NIM, DeepSeek, Requesty, Z.ai, Moonshot, your own server)", name: compatible},
	{label: "Ollama on this machine (no key)", name: "ollama"},
}

const compatible = "(compatible)"

var compatibleChoices = []providerChoice{
	{label: "OpenRouter", name: "openrouter"},
	{label: "NVIDIA NIM", name: "nvidia"},
	{label: "DeepSeek", name: "deepseek"},
	{label: "Requesty", name: "requesty"},
	{label: "Z.ai (GLM)", name: "zai"},
	{label: "Moonshot (Kimi)", name: "moonshot"},
	{label: "Another server (vLLM, LM Studio, a gateway): its address", name: ""},
}

// anthropicAPI is where Anthropic's models are listed; tests replace it.
var anthropicAPI = "https://api.anthropic.com"

func (s *setup) chooseProvider() error {
	choice, err := s.pick("Which models do you want to use?", mainChoices)
	if err == nil && choice.name == compatible {
		choice, err = s.pick("Which service?", compatibleChoices)
	}
	if err != nil {
		return err
	}
	s.provider = choice
	if choice.name == "" {
		return s.askServer()
	}
	s.endpoint, err = (&config{}).endpointNamed(choice.name)
	if base, ok := s.c.setupBases[choice.name]; ok {
		s.endpoint.base = base
	}
	return err
}

func (s *setup) pick(question string, choices []providerChoice) (providerChoice, error) {
	labels := make([]string, len(choices))
	for i, p := range choices {
		labels[i] = p.label
	}
	i, err := s.choose(question, labels)
	if err != nil {
		return providerChoice{}, err
	}
	return choices[i], nil
}

// checkServer accepts an https address, an http one on this machine, or
// an http one elsewhere the user allows (the key travels in clear).
func (s *setup) checkServer(v string) error {
	u, err := parseBase(v)
	if err != nil {
		return err
	}
	if u.Scheme == "https" || safehttp.Loopback(u.Hostname()) {
		return nil
	}
	yes, err := s.p.Confirm("Plain http to another machine: the key and every message travel unencrypted. Allow it?", false)
	if err != nil || !yes {
		return errors.New(ErrUsage, "give an https:// address, or one on this machine")
	}
	s.allowHTTP = true
	return nil
}

// askServer asks another server's address, whether plain http to another
// host may carry the key (the CLI then allows it: allow_http), and
// whether it needs a key at all. The provider is named after the server's
// host, its key after the provider.
func (s *setup) askServer() error {
	base, err := s.ask("The server's address (ending in /v1)", "http://127.0.0.1:8000/v1", s.checkServer)
	if err != nil {
		return err
	}
	name := freeName(serverName(base), s.takenNames())
	s.endpoint = endpoint{name: name, kind: providerOpenAI, base: base, keyName: keyNameOf(name)}
	s.custom = true
	needs, err := s.p.Confirm("Does the server need an API key?", true)
	if err == nil && !needs {
		s.endpoint.keyName = ""
	}
	return err
}

// serverName names a server of one's own after its host (vllm.lan:8000
// is vllm-lan-8000, 10.0.0.5 server-10-0-0-5), never as a built-in
// provider.
func serverName(base string) string {
	u, _ := url.Parse(base) // checked by checkServer
	name := dashed(strings.ToLower(u.Host))
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		name = "server-" + name // an address: its key's name cannot start with a digit
	}
	name = strings.TrimSuffix(name[:min(len(name), 58)], "-")
	if _, builtin := builtinProviders[name]; builtin || builtinKey(keyNameOf(name)) {
		name += "-own" // neither a vendor's name nor its key (audit 23/11: gemini, GEMINI_API_KEY)
	}
	return name
}

// dashed keeps letters and digits, one dash for each run of anything else.
func dashed(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// takenNames are the providers of the file and their keys' names: a
// server added must take neither (audit 23/11: its key overwrote another
// provider's in the store, and went to that provider).
func (s *setup) takenNames() map[string]bool {
	taken := map[string]bool{}
	w, err := argus.NewConfigWriter(s.path, argus.FormatYAML, nil)
	if err != nil {
		return taken // no file yet, or one the write step will refuse
	}
	providers, _ := w.GetValue("providers").(map[string]interface{})
	for name, entry := range providers {
		taken[strings.ToUpper(keyNameOf(name))] = true
		if e, ok := entry.(map[string]interface{}); ok {
			if k, ok := e["key_env"].(string); ok {
				taken[strings.ToUpper(k)] = true
			}
		}
	}
	return taken
}

// freeName is name, or name-2, name-3..., whose key name is not taken.
func freeName(name string, taken map[string]bool) string {
	out := name
	for i := 2; taken[strings.ToUpper(keyNameOf(out))]; i++ {
		out = strings.TrimSuffix(name[:min(len(name), 58)], "-") + "-" + strconv.Itoa(i)
	}
	return out
}

// keyNameOf is the secret of a provider's key: VLLM_LAN_API_KEY.
func keyNameOf(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_API_KEY"
}

// askKey asks the key (unless the server needs none) and checks it by
// listing the models; a refused key is asked again.
func (s *setup) askKey() error {
	if s.endpoint.keyName == "" {
		return nil
	}
	for range 3 {
		key, err := s.p.AskSecret("Your " + s.label() + " API key (not shown, kept in the store you chose)")
		if err != nil {
			return err
		}
		s.key = key
		_, err = s.models()
		if errors.HasCode(err, errKeyRefused) {
			if lerr := s.c.line("  the key was refused: check it and paste it again"); lerr != nil {
				return lerr
			}
			continue
		}
		if err != nil { // not refused, not checked: the server could not list its models
			keep, cerr := s.p.Confirm("The key could not be checked ("+describe(err)+"). Keep it anyway? (the last step tries it)", true)
			if cerr != nil || !keep {
				return errors.New(ErrUsage, "setup stopped: the key was not kept")
			}
		}
		return s.keys.Set(s.ctx, s.endpoint.keyName, key)
	}
	return errors.New(ErrUsage, "the key was refused three times")
}

const errKeyRefused errors.ErrorCode = "SETUP_KEY_REFUSED"

// models lists the endpoint's models, newest names first.
func (s *setup) models() ([]string, error) {
	if s.endpoint.kind == providerChatGPT {
		return s.chatgptModels()
	}
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	req, err := s.modelsRequest(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return nil, errors.New(ErrUsage, "the server did not answer")
	}
	defer func() { _ = resp.Body.Close() }() // #nosec G104 -- read below
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errors.New(errKeyRefused, "the key was refused")
	}
	var out struct {
		Data []listedModel `json:"data"`
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(ErrUsage, "the server answered "+strconv.Itoa(resp.StatusCode))
	}
	if json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(&out) != nil {
		return nil, errors.New(ErrUsage, "the server's list of models is not one")
	}
	return modelIDs(out.Data), nil
}

// listedModel is a model as /models lists it: OpenAI's "created" (Unix
// seconds), Anthropic's "created_at" (RFC 3339).
type listedModel struct {
	ID        string `json:"id"`
	Created   int64  `json:"created"`
	CreatedAt string `json:"created_at"`
}

func (m listedModel) when() int64 {
	if m.Created > 0 {
		return m.Created
	}
	if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		return t.Unix()
	}
	return 0
}

// modelsRequest is the listing's request, with the key as the API wants.
func (s *setup) modelsRequest(ctx context.Context) (*http.Request, error) {
	url := strings.TrimRight(s.endpoint.base, "/") + "/models"
	if s.endpoint.kind == providerAnthropic {
		url = anthropicAPI + "/v1/models?limit=100"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if s.endpoint.kind == providerAnthropic {
		req.Header.Set("x-api-key", s.key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else if s.key != "" {
		req.Header.Set("Authorization", "Bearer "+s.key)
	}
	return req, nil
}

// modelIDs are the listed models, newest first (by their date, in the
// server's order when it gives none).
func modelIDs(data []listedModel) []string {
	sort.SliceStable(data, func(i, j int) bool { return data[i].when() > data[j].when() })
	ids := make([]string, 0, len(data))
	for _, m := range data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// notChat are words in the names of models that do not chat: offered as
// the chat model they would fail at the first message.
var notChat = []string{"embed", "whisper", "tts", "dall-e", "moderation", "transcribe", "image", "audio", "realtime", "search", "rerank"}

func chatModels(ids []string) []string {
	var out []string
	for _, id := range ids {
		low := strings.ToLower(id)
		if !slices.ContainsFunc(notChat, func(w string) bool { return strings.Contains(low, w) }) {
			out = append(out, id)
		}
	}
	return out
}

// chooseModel offers the endpoint's models (the first 15, or a name
// typed); when they cannot be listed, asks for the name.
func (s *setup) chooseModel() error {
	ids, err := s.models()
	ids = chatModels(ids)
	if err != nil || len(ids) == 0 {
		s.model, err = s.ask("The model's name", s.provider.model, nonEmpty("a model name"))
		return err
	}
	shown := ids[:min(len(ids), 15)]
	i, err := s.choose("Which model?", append(append([]string(nil), shown...), "another (type its name)"))
	if err != nil {
		return err
	}
	if i < len(shown) {
		s.model = shown[i]
		return nil
	}
	s.model, err = s.ask("The model's name", "", nonEmpty("a model name"))
	return err
}

func nonEmpty(what string) func(string) error {
	return func(v string) error {
		if v == "" {
			return errors.New(ErrUsage, "give "+what)
		}
		return nil
	}
}

// tryModel asks the model one short question with the new settings.
func (s *setup) tryModel() error {
	ctx, cancel := context.WithTimeout(s.ctx, 60*time.Second)
	defer cancel()
	p, err := s.newProvider()
	if err != nil {
		return err
	}
	req := &agent.Request{Messages: []agent.Message{agent.UserText("Reply with the single word: ready")}, MaxTokens: 64}
	resp, err := p.Generate(ctx, req, nil)
	if err != nil {
		return errors.Wrap(err, ErrUsage, "ringil.yaml is written, but the model did not answer: "+describe(err))
	}
	return s.c.line("  the model answered: " + short(strings.TrimSpace(resp.Message.Text())))
}

func (s *setup) newProvider() (agent.Provider, error) {
	key := func(context.Context) (string, error) { return s.key, nil }
	switch s.endpoint.kind {
	case providerAnthropic:
		return s.c.newAnthropic(anthropic.Config{Model: s.model, KeyFunc: key})
	case providerChatGPT:
		return chatgpt.New(chatgpt.Config{Model: s.model, Session: s.session, BaseURL: s.c.chatgptBase})
	}
	return s.c.newProvider(openai.Config{BaseURL: s.endpoint.base, Model: s.model, KeyFunc: key, AllowHTTP: s.allowHTTP})
}

// label names the chosen provider to the user.
func (s *setup) label() string {
	if s.custom {
		return s.endpoint.name
	}
	return s.provider.label
}
