// config.go: ringil.yaml, the one file that sets ringil up
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"
	"github.com/agilira/ringil-agent-sdk/channel"
)

// config is ringil.yaml. A setting given as a flag wins for that command;
// the model, base and embedding model in the environment (RINGIL_MODEL,
// RINGIL_BASE_URL, RINGIL_EMBED_MODEL) win over the file. Keys are the
// global flags' names with _ for -. Secrets are never in it: key_env and
// ${NAME} name the variables that hold them.
type config struct {
	Model          string   `argus:"model"`
	Base           string   `argus:"base"`
	KeyEnv         string   `argus:"key_env"`
	EmbedModel     string   `argus:"embed_model"`
	Instructions   string   `argus:"instructions"`
	MaxTurns       int      `argus:"max_turns"`
	ContextWindow  int      `argus:"context_window"`
	Summarize      bool     `argus:"summarize"`
	AllowHTTP      bool     `argus:"allow_http"`
	AllowDomains   []string `argus:"allow_domains"`
	NoTools        bool     `argus:"no_tools"`
	NoMemory       bool     `argus:"no_memory"`
	NoApprovals    bool     `argus:"no_approvals"`
	AcceptEdits    bool     `argus:"accept_edits"`
	WorkflowCalls  int      `argus:"workflow_calls"`
	WorkflowTokens int      `argus:"workflow_tokens"`
	SessionsDir    string   `argus:"sessions_dir"`
	// Images: the model reads images (RINGIL_IMAGES=1 too).
	Images bool `argus:"images"`
	// EmbedInputType sends the input type to the embedding model
	// (RINGIL_EMBED_INPUT_TYPE=1 too).
	EmbedInputType bool        `argus:"embed_input_type"`
	Transcribe     audioConfig `argus:"transcribe"`
	Speech         audioConfig `argus:"speech"`
	// AuditFile is where every revision of the configuration is recorded
	// (default <config>/ringil/audit.jsonl; a .db file is SQLite, in a
	// build with cgo); NoAudit records none.
	AuditFile string `argus:"audit_file"`
	NoAudit   bool   `argus:"no_audit"`

	// Of the revision, not of the file: the instructions' text by path,
	// and the revision's digest (file, instructions and skills).
	texts  map[string]string
	digest string

	Profiles   map[string]profile     `argus:"profiles"`
	MCPServers map[string]mcpEntry    `argus:"mcp_servers"`
	Agents     map[string]remoteEntry `argus:"agents"`
	Serve      serveConfig            `argus:"serve"`
}

// audioConfig is a model that hears voice messages (transcribe) or speaks
// the answers (speech): RINGIL_TRANSCRIBE_* and RINGIL_SPEECH_* in the
// environment win over it. At another endpoint than the chat model's it
// gets only its own key, KeyEnv (default <PREFIX>_API_KEY).
type audioConfig struct {
	Model  string `argus:"model"`
	Base   string `argus:"base"`
	KeyEnv string `argus:"key_env"`
	Voice  string `argus:"voice"` // speech only
}

// serveConfig is what `ringil serve` does beside running agents.
type serveConfig struct {
	Listen        string         `argus:"listen"`
	MaxRuns       int            `argus:"max_runs"`
	A2A           bool           `argus:"a2a"`
	NoTriggers    bool           `argus:"no_triggers"`
	Webhooks      string         `argus:"webhooks"`
	Language      string         `argus:"language"`
	MemberProfile string         `argus:"member_profile"`
	PublicProfile string         `argus:"public_profile"`
	MaxPerHour    *int           `argus:"max_per_hour"`
	NewAfter      string         `argus:"new_after"`
	Replies       string         `argus:"replies"`
	Telegram      telegramConfig `argus:"telegram"`
}

// telegramConfig is who the Telegram bot talks with; its token is
// RINGIL_TELEGRAM_TOKEN.
type telegramConfig struct {
	Owners       []string `argus:"owners"`
	Members      []string `argus:"members"`
	CompanyGroup string   `argus:"company_group"`
	MemberPhones []string `argus:"member_phones"`
}

// Defaults of serve, for what the file leaves out.
const (
	defaultListen     = "127.0.0.1:8765"
	defaultMaxRuns    = 4
	defaultMaxPerHour = 30
)

func (s serveConfig) listen() string {
	if s.Listen == "" {
		return defaultListen
	}
	return s.Listen
}

func (s serveConfig) maxRuns() int {
	if s.MaxRuns == 0 {
		return defaultMaxRuns
	}
	return s.MaxRuns
}

func (s serveConfig) maxPerHour() int {
	if s.MaxPerHour == nil {
		return defaultMaxPerHour
	}
	return *s.MaxPerHour
}

func (s serveConfig) language() string {
	if s.Language == "" {
		return "en"
	}
	return s.Language
}

func (s serveConfig) replies() string {
	if s.Replies == "" {
		return "voice"
	}
	return s.Replies
}

func (s serveConfig) newAfter() string {
	if s.NewAfter == "" {
		return "24h"
	}
	return s.NewAfter
}

// checkPaths refuses an instructions path that would read as a pattern,
// and an audio key_env that is no variable name.
func (f *config) checkPaths() error {
	paths := []string{f.Instructions}
	for _, name := range names(f.Profiles) {
		paths = append(paths, f.Profiles[name].Instructions)
	}
	for _, p := range paths {
		if strings.ContainsAny(p, "*?[") {
			return errors.New(ErrUsage, "an instructions file's path cannot hold * ? or [: "+p)
		}
	}
	for _, a := range []audioConfig{f.Transcribe, f.Speech} {
		if a.KeyEnv != "" && validKeyName(a.KeyEnv) != nil {
			return errors.New(ErrUsage, "transcribe and speech: key_env is not a valid variable name: "+a.KeyEnv)
		}
	}
	return nil
}

// shortDigest names the revision in a log line.
func (f *config) shortDigest() string {
	return f.digest[:min(12, len(f.digest))]
}

// people is who the chats talk with: changed while serve runs.
// noApprovals says whether the owners' runs in the chats ask nothing.
func (s serveConfig) people(noApprovals bool) channel.People {
	return channel.People{Owners: s.Telegram.Owners, Members: s.Telegram.Members, MemberProfile: s.MemberProfile,
		MemberPhones: s.Telegram.MemberPhones, PublicProfile: s.PublicProfile, MaxPerHour: s.maxPerHour(),
		NoApprovals: noApprovals}
}

// fixed is what serve sets up once: all but people.
func (s serveConfig) fixed() serveConfig {
	s.Telegram.Owners, s.Telegram.Members, s.Telegram.MemberPhones = nil, nil, nil
	s.MemberProfile, s.PublicProfile, s.MaxPerHour = "", "", nil
	return s
}

// Validate is what a file must be to be applied at all: a reload that
// fails it is refused and the last good file stays.
func (f *config) Validate() error {
	for _, name := range names(f.Profiles) {
		if err := f.Profiles[name].check(name); err != nil {
			return err
		}
	}
	for _, name := range []string{f.Serve.MemberProfile, f.Serve.PublicProfile} {
		if err := checkChatProfile(f.Profiles, name); err != nil {
			return err
		}
	}
	if err := f.checkPaths(); err != nil {
		return err
	}
	for _, name := range sortedKeys(f.MCPServers) {
		if err := f.MCPServers[name].check(); err != nil {
			return errors.New(ErrUsage, "mcp_servers."+name+": "+err.Error())
		}
	}
	return f.Serve.check()
}

func (s serveConfig) check() error {
	switch {
	case !slices.Contains([]string{"", "voice", "text"}, s.Replies):
		return errors.New(ErrUsage, "serve.replies is voice or text")
	case !slices.Contains([]string{"", "en", "it"}, s.Language):
		return errors.New(ErrUsage, "serve.language is en or it")
	case s.MaxRuns < 0:
		return errors.New(ErrUsage, "serve.max_runs cannot be negative")
	case s.NoTriggers && s.Webhooks != "":
		return errors.New(ErrUsage, "serve.webhooks sets off triggers: it does not go with serve.no_triggers")
	}
	_, err := newAfter(s.newAfter())
	return err
}

// from makes the file's relative paths absolute, from dir (the file's).
func (f config) from(dir string) *config {
	f.Instructions = fromDir(dir, f.Instructions)
	f.SessionsDir = fromDir(dir, f.SessionsDir)
	profiles := make(map[string]profile, len(f.Profiles))
	for name, p := range f.Profiles {
		profiles[name] = p.from(dir)
	}
	f.Profiles = profiles
	return &f
}

// fromDir is path from dir, when relative.
func fromDir(dir, path string) string {
	if path == "" || rooted(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// restartFor names what changed that serve set up once, at its start.
func restartFor(old, next *config) string {
	var changed []string
	if !reflect.DeepEqual(old.MCPServers, next.MCPServers) {
		changed = append(changed, "mcp_servers")
	}
	if !reflect.DeepEqual(old.Agents, next.Agents) {
		changed = append(changed, "agents")
	}
	if !reflect.DeepEqual(old.Serve.fixed(), next.Serve.fixed()) {
		changed = append(changed, "serve")
	}
	if old.AllowHTTP != next.AllowHTTP || old.KeyEnv != next.KeyEnv {
		changed = append(changed, "allow_http and key_env for the chats' voice")
	}
	if !sameKeys(old.Profiles, next.Profiles) {
		changed = append(changed, "the list of profiles")
	}
	return strings.Join(changed, ", ")
}

func sameKeys(a, b map[string]profile) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// Global settings: the flag when given, else the file, else the flag's
// default.

func (c *cli) flagString(ctx *orpheus.Context, name string) string {
	if !ctx.GlobalFlagChanged(name) {
		if v := c.conf().stringOf(name); v != "" {
			return v
		}
	}
	return ctx.GetGlobalFlagString(name)
}

func (c *cli) flagBool(ctx *orpheus.Context, name string) bool {
	if ctx.GlobalFlagChanged(name) {
		return ctx.GetGlobalFlagBool(name)
	}
	return c.conf().boolOf(name) || ctx.GetGlobalFlagBool(name)
}

func (c *cli) flagInt(ctx *orpheus.Context, name string) int {
	if !ctx.GlobalFlagChanged(name) {
		if v := c.conf().intOf(name); v != 0 {
			return v
		}
	}
	return ctx.GetGlobalFlagInt(name)
}

func (f *config) stringOf(name string) string {
	switch name {
	case "model":
		return f.Model
	case "base":
		return f.Base
	case "key-env":
		return f.KeyEnv
	case "embed-model":
		return f.EmbedModel
	case "instructions":
		return f.Instructions
	case "sessions-dir":
		return f.SessionsDir
	case "allow-domains":
		return strings.Join(f.AllowDomains, ",")
	}
	return ""
}

func (f *config) boolOf(name string) bool {
	return map[string]bool{
		"summarize": f.Summarize, "allow-http": f.AllowHTTP, "no-tools": f.NoTools,
		"no-memory": f.NoMemory, "no-approvals": f.NoApprovals, "accept-edits": f.AcceptEdits,
	}[name]
}

func (f *config) intOf(name string) int {
	return map[string]int{
		"max-turns": f.MaxTurns, "context-window": f.ContextWindow,
		"workflow-calls": f.WorkflowCalls, "workflow-tokens": f.WorkflowTokens,
	}[name]
}

// protectedFiles are written by the file tools only with a yes:
// ringil.yaml wherever it is and every file of instructions, so that the
// agent does not rewrite its own rules unasked.
func (c *cli) protectedFiles(ctx *orpheus.Context) []string {
	f := c.conf()
	paths := []string{c.configName(), c.flagString(ctx, "instructions")}
	for _, name := range names(f.Profiles) {
		paths = append(paths, f.Profiles[name].Instructions)
	}
	return slices.DeleteFunc(paths, func(p string) bool { return p == "" })
}

// withConfig loads ringil.yaml before a command runs.
func (c *cli) withConfig(run func(*orpheus.Context) error) func(*orpheus.Context) error {
	return func(ctx *orpheus.Context) error {
		if err := c.loadConfig(ctx); err != nil {
			return err
		}
		defer c.settings.close() // whatever the command: the audit is flushed
		return run(ctx)
	}
}
