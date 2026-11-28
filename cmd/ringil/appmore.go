// appmore.go: chat apps, memory, Composio, remote agents and the restart,
// from the app
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/a2a"
	"github.com/agilira/ringil-agent-sdk/mcp"
	"github.com/agilira/ringil-agent-sdk/mcp/composio"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/serve"
)

// The rest of the settings an app changes, mounted with them:
//
//	GET    /v1/settings/channels              Telegram and Slack: who talks with the agent, which agents answer, whether each runs
//	PUT    /v1/settings/channels              the same fields; the tokens go to /v1/settings/secrets (RINGIL_TELEGRAM_TOKEN, SLACK_BOT_TOKEN, SLACK_APP_TOKEN)
//	GET    /v1/settings/memory                where the memory is kept
//	PUT    /v1/settings/memory                {"kind":"sqlite"|"chroma"|"mem0","url"?,...}: the next runs use it
//	GET    /v1/memories?q=&limit=             the user's memories: the latest, or those about q
//	POST   /v1/memories                       {"content"} remember one
//	DELETE /v1/memories/{id}                  forget one
//	GET    /v1/settings/composio              whether Composio is set up
//	PUT    /v1/settings/composio              {"key"} check the consumer key, keep it, add the server
//	DELETE /v1/settings/composio              remove the server and the key
//	GET    /v1/settings/remote_agents         the remote agents (A2A) the agent may ask
//	PUT    /v1/settings/remote_agents/{name}  {"url","headers"?,"allow_other_origin"?,"disabled"?}: read its card, then save
//	DELETE /v1/settings/remote_agents/{name}  remove one
//	PUT    /v1/settings/a2a                   {"expose"}: offer the main agent to other agents
//	POST   /v1/restart                        start serve again, for what only a start takes (GET /v1/settings says what)
func (c *cli) mountMore(octx *orpheus.Context, srv *serve.Server) {
	routes := map[string]http.HandlerFunc{
		"GET /v1/settings/channels":                c.getChannels(octx),
		"PUT /v1/settings/channels":                c.putChannels,
		"GET /v1/settings/memory":                  c.getMemorySettings,
		"PUT /v1/settings/memory":                  c.putMemorySettings,
		"GET /v1/memories":                         c.listMemories(octx),
		"POST /v1/memories":                        c.addMemory(octx),
		"DELETE /v1/memories/{id}":                 c.forgetMemory(octx),
		"GET /v1/settings/composio":                c.getComposio(octx),
		"PUT /v1/settings/composio":                c.putComposio(octx),
		"DELETE /v1/settings/composio":             c.deleteComposio(octx),
		"GET /v1/settings/remote_agents":           c.getRemoteAgents,
		"PUT /v1/settings/remote_agents/{name}":    c.putRemoteAgent(octx),
		"DELETE /v1/settings/remote_agents/{name}": c.deleteRemoteAgent,
		"PUT /v1/settings/a2a":                     c.putA2A,
		"POST /v1/restart":                         c.restartServe(srv),
	}
	for pattern, h := range routes {
		srv.Mount(pattern, h)
	}
}

// Chat apps.

// ChannelsIO is who talks with the agent in the chat apps.
type ChannelsIO struct {
	Telegram      TelegramIO `json:"telegram"`
	Slack         SlackIO    `json:"slack"`
	MemberProfile string     `json:"member_profile,omitempty"`
	PublicProfile string     `json:"public_profile,omitempty"`
	Replies       string     `json:"replies,omitempty"`
	NewAfter      string     `json:"new_after,omitempty"`
	MaxPerHour    *int       `json:"max_per_hour,omitempty"`
	Language      string     `json:"language,omitempty"`
}

// TelegramIO is Telegram's people; TokenSet and Running are read only.
type TelegramIO struct {
	Owners       []string `json:"owners"`
	Members      []string `json:"members"`
	CompanyGroup string   `json:"company_group,omitempty"`
	MemberPhones []string `json:"member_phones"`
	TokenSet     bool     `json:"token_set,omitempty"`
	Running      bool     `json:"running,omitempty"`
}

// SlackIO is Slack's people; TokenSet and Running are read only.
type SlackIO struct {
	Owners   []string `json:"owners"`
	Members  []string `json:"members"`
	TokenSet bool     `json:"token_set,omitempty"`
	Running  bool     `json:"running,omitempty"`
}

func (c *cli) getChannels(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := c.conf().Serve
		out := ChannelsIO{MemberProfile: s.MemberProfile, PublicProfile: s.PublicProfile, Replies: s.Replies, NewAfter: s.NewAfter,
			MaxPerHour: s.MaxPerHour, Language: s.Language,
			Telegram: TelegramIO{Owners: nonNil(s.Telegram.Owners), Members: nonNil(s.Telegram.Members), CompanyGroup: s.Telegram.CompanyGroup,
				MemberPhones: nonNil(s.Telegram.MemberPhones), Running: c.telegramBridge() != nil},
			Slack: SlackIO{Owners: nonNil(s.Slack.Owners), Members: nonNil(s.Slack.Members), Running: c.slackBridge != nil && c.slackBridge.Load() != nil}}
		if res, err := c.resolver(octx); err == nil {
			out.Telegram.TokenSet = has(r.Context(), storeOnly(res), "RINGIL_TELEGRAM_TOKEN")
			out.Slack.TokenSet = has(r.Context(), storeOnly(res), "SLACK_BOT_TOKEN") && has(r.Context(), storeOnly(res), "SLACK_APP_TOKEN")
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// has: the secret name is set, and not empty.
func has(ctx context.Context, res secrets.Resolver, name string) bool {
	v, found, err := secrets.Lookup(ctx, res, name)
	return err == nil && found && v != ""
}

// putChannels writes who talks with the agent: the running chats take it
// at once (a chat app not running yet starts with serve).
func (c *cli) putChannels(w http.ResponseWriter, r *http.Request) {
	var in ChannelsIO
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Telegram.TokenSet || in.Telegram.Running || in.Slack.TokenSet || in.Slack.Running {
		fail(w, http.StatusBadRequest, "SETTINGS_INPUT", "token_set and running are read, not written: the tokens go to /v1/settings/secrets")
		return
	}
	c.change(w, func(next *config) *refusal {
		s := &next.Serve
		s.Telegram.Owners, s.Telegram.Members, s.Telegram.CompanyGroup, s.Telegram.MemberPhones = in.Telegram.Owners, in.Telegram.Members, in.Telegram.CompanyGroup, in.Telegram.MemberPhones
		s.Slack.Owners, s.Slack.Members = in.Slack.Owners, in.Slack.Members
		s.MemberProfile, s.PublicProfile, s.Replies, s.NewAfter, s.MaxPerHour, s.Language = in.MemberProfile, in.PublicProfile, in.Replies, in.NewAfter, in.MaxPerHour, in.Language
		return c.checkChannels(next)
	}, func(ed *edit) {
		ed.set("serve.telegram.owners", listOrNil(in.Telegram.Owners))
		ed.set("serve.telegram.members", listOrNil(in.Telegram.Members))
		ed.set("serve.telegram.company_group", in.Telegram.CompanyGroup)
		ed.set("serve.telegram.member_phones", listOrNil(in.Telegram.MemberPhones))
		ed.set("serve.slack.owners", listOrNil(in.Slack.Owners))
		ed.set("serve.slack.members", listOrNil(in.Slack.Members))
		ed.set("serve.member_profile", in.MemberProfile)
		ed.set("serve.public_profile", in.PublicProfile)
		ed.set("serve.replies", in.Replies)
		ed.set("serve.new_after", in.NewAfter)
		if in.MaxPerHour != nil {
			ed.set("serve.max_per_hour", *in.MaxPerHour)
		} else {
			ed.set("serve.max_per_hour", nil)
		}
		ed.set("serve.language", in.Language)
	})
}

func (c *cli) checkChannels(next *config) *refusal {
	s := next.Serve
	hasMembers := len(s.Telegram.Members) > 0 || len(s.Telegram.MemberPhones) > 0 || s.Telegram.CompanyGroup != "" || len(s.Slack.Members) > 0
	if hasMembers && s.MemberProfile == "" {
		return &refusal{status: http.StatusBadRequest, code: "SETTINGS_INPUT", what: "member_profile is required when members, member_phones or company_group are set"}
	}
	if r := checkChannelPhones(s.Telegram.MemberPhones); r != nil {
		return r
	}
	return c.checkRunningPeople(next)
}

func checkChannelPhones(phones []string) *refusal {
	for _, p := range phones {
		if !validMemberPhone(p) {
			return &refusal{status: http.StatusBadRequest, code: "SETTINGS_INPUT", what: "member_phones entries must include country code (+ or 00) and 8-15 digits"}
		}
	}
	return nil
}

func validMemberPhone(s string) bool {
	if strings.Contains(s, "(0)") {
		return false
	}
	cleaned := stripPhonePunct(s)
	rest, ok := strings.CutPrefix(cleaned, "+")
	if !ok {
		rest, ok = strings.CutPrefix(cleaned, "00")
	}
	return ok && isE164Digits(rest)
}

func stripPhonePunct(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '.', '(', ')':
			return -1
		}
		return r
	}, s)
}

func isE164Digits(s string) bool {
	return len(s) >= 8 && len(s) <= 15 && s[0] != '0' && strings.Trim(s, "0123456789") == ""
}

func (c *cli) checkRunningPeople(next *config) *refusal {
	no := c.settings != nil && c.settings.noApprovals != nil && c.settings.noApprovals(next)
	var all []peopleFor
	if b := c.telegramBridge(); b != nil {
		all = append(all, peopleFor{b, next.Serve.people(no)})
	}
	if b := c.slackOf(); b != nil {
		all = append(all, peopleFor{b, next.Serve.slackPeople(no)})
	}
	for _, p := range all {
		if err := p.bridge.CheckPeople(p.people); err != nil {
			return &refusal{status: http.StatusBadRequest, code: "SETTINGS_INPUT", what: describe(err)}
		}
	}
	return nil
}

// Memory.

// MemoryIO is where the memory is kept, and the embedding model of the
// main provider for memory by meaning.
type MemoryIO struct {
	Kind             string            `json:"kind,omitempty"` // sqlite (default), chroma, mem0
	URL              string            `json:"url,omitempty"`
	Tenant           string            `json:"tenant,omitempty"`
	Database         string            `json:"database,omitempty"`
	CollectionPrefix string            `json:"collection_prefix,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	Raw              bool              `json:"raw,omitempty"`
	EmbedModel       string            `json:"embed_model,omitempty"`
}

func (c *cli) getMemorySettings(w http.ResponseWriter, _ *http.Request) {
	f := c.conf()
	m := f.Memory
	writeJSON(w, http.StatusOK, MemoryIO{Kind: or(m.Kind, "sqlite"), URL: m.URL, Tenant: m.Tenant, Database: m.Database,
		CollectionPrefix: m.Prefix, Headers: m.Headers, Raw: m.Raw, EmbedModel: f.EmbedModel})
}

func (c *cli) putMemorySettings(w http.ResponseWriter, r *http.Request) {
	var in MemoryIO
	if !decodeJSON(w, r, &in) {
		return
	}
	if msg := badHeaders(in.Headers); msg != "" {
		fail(w, http.StatusBadRequest, "SETTINGS_INPUT", msg)
		return
	}
	kind := in.Kind
	if kind == "sqlite" {
		kind = "" // the default
	}
	c.change(w, func(next *config) *refusal {
		next.Memory = memoryStore{Kind: kind, URL: in.URL, Tenant: in.Tenant, Database: in.Database, Prefix: in.CollectionPrefix, Headers: in.Headers, Raw: in.Raw}
		next.EmbedModel = in.EmbedModel
		return nil
	}, func(ed *edit) {
		ed.set("memory_store", nil)
		ed.merge("memory_store", "kind", kind, "url", in.URL, "tenant", in.Tenant, "database", in.Database, "collection_prefix", in.CollectionPrefix)
		ed.set("memory_store.headers", headerMap(in.Headers))
		ed.set("memory_store.raw", trueOrNone(in.Raw))
		ed.set("embed_model", in.EmbedModel)
	})
}

// headerName is an HTTP header's name (RFC 9110's token).
var headerName = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,128}$")

// isReference: v names a secret, ${NAME}, and holds nothing else.
func isReference(v string) bool {
	name, ok := strings.CutPrefix(v, "${")
	if !ok {
		return false
	}
	name, ok = strings.CutSuffix(name, "}")
	return ok && secrets.ValidateName(name) == nil
}

// withMemory runs fn on the user's own memory, as the main agent's runs
// use it now; none set up, it says so.
func (c *cli) withMemory(w http.ResponseWriter, octx *orpheus.Context, fn func(agent.Memory)) {
	r := c.forRun()
	r.settings = c.settings.pinned()
	defer r.shutdown()
	mem, err := r.memory(octx)
	switch {
	case err != nil:
		fail(w, http.StatusBadGateway, "MEMORY", describe(err))
	case mem == nil:
		fail(w, http.StatusConflict, "MEMORY", "the memory is off (--no-memory)")
	default:
		fn(mem)
	}
}

func (c *cli) listMemories(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 500 {
				fail(w, http.StatusBadRequest, "SETTINGS_INPUT", "limit is 1 to 500")
				return
			}
			limit = n
		}
		c.withMemory(w, octx, func(mem agent.Memory) {
			recs, err := mem.Search(r.Context(), r.URL.Query().Get("q"), limit)
			if err != nil && !errors.HasCode(err, agent.ErrMemoryPartial) {
				fail(w, http.StatusBadGateway, "MEMORY", describe(err))
				return
			}
			if recs == nil {
				recs = []agent.MemoryRecord{}
			}
			writeJSON(w, http.StatusOK, recs)
		})
	}
}

type memoryIn struct {
	Content string `json:"content"`
}

func (c *cli) addMemory(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in memoryIn
		if !decodeJSON(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.Content) == "" || len(in.Content) > agent.MaxMemoryContent {
			fail(w, http.StatusBadRequest, "SETTINGS_INPUT", "a memory is 1 byte to 32 KiB of text")
			return
		}
		c.withMemory(w, octx, func(mem agent.Memory) {
			rec, err := mem.Put(r.Context(), agent.MemoryRecord{Content: in.Content})
			if err != nil {
				fail(w, http.StatusBadGateway, "MEMORY", describe(err))
				return
			}
			writeJSON(w, http.StatusCreated, rec)
		})
	}
}

func (c *cli) forgetMemory(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.withMemory(w, octx, func(mem agent.Memory) {
			if err := mem.Delete(r.Context(), r.PathValue("id")); err != nil {
				code := http.StatusBadGateway
				if errors.HasCode(err, agent.ErrMemoryNotFound) {
					code = http.StatusNotFound
				}
				fail(w, code, "MEMORY", describe(err))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

// Composio.

// ComposioOut is whether Composio is set up.
type ComposioOut struct {
	KeySet bool   `json:"key_set"`
	Server string `json:"server,omitzero"` // the MCP server that reaches it
}

func (c *cli) getComposio(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := ComposioOut{Server: c.composioServer()}
		if res, err := c.resolver(octx); err == nil {
			out.KeySet = has(r.Context(), res, composioKey)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// composioServer is the MCP server at Composio Connect, "" for none.
func (c *cli) composioServer() string {
	return composioServerIn(c.conf())
}

func composioServerIn(f *config) string {
	for _, name := range sortedKeys(f.MCPServers) {
		if isComposio(f.MCPServers[name].URL) {
			return name
		}
	}
	return ""
}

type composioIn struct {
	Key string `json:"key"`
}

// putComposio checks the consumer key with Composio, keeps it in the
// secrets store, and adds the server, which the next runs use.
func (c *cli) putComposio(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in composioIn
		if !decodeJSON(w, r, &in) {
			return
		}
		key := strings.TrimSpace(in.Key)
		if err := checkComposioKey(r.Context(), key); err != nil {
			fail(w, http.StatusBadRequest, "SETTINGS_COMPOSIO", describe(err))
			return
		}
		st, _, ok := c.secretStoreNamed(w, r, octx, composioKey)
		if !ok {
			return
		}
		if err := st.Set(r.Context(), composioKey, key); err != nil {
			fail(w, http.StatusInternalServerError, "SETTINGS_SECRETS", describe(err))
			return
		}
		if err := c.addComposio(r.Context(), octx); err != nil {
			fail(w, http.StatusBadRequest, "SETTINGS_MCP", describe(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// checkComposioKey asks Composio whether it takes key.
func checkComposioKey(ctx context.Context, key string) error {
	if !strings.HasPrefix(key, "ck_") || len(key) > 512 {
		return errors.New(ErrUsage, "a Composio consumer key starts with ck_ (composio.dev, Settings)")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := mcp.Connect(ctx, composio.Connect(func(context.Context) (string, error) { return key, nil }))
	if err != nil {
		return errors.Wrap(err, ErrUsage, "Composio did not take the key")
	}
	_ = conn.Close()
	return nil
}

// addComposio adds the server at Composio Connect, with the kept key,
// unless one is there.
func (c *cli) addComposio(ctx context.Context, octx *orpheus.Context) error {
	if c.settings == nil || c.settings.path == "" {
		return errors.New(ErrUsage, "serve runs without a configuration directory: there is no ringil.yaml to change")
	}
	if c.composioServer() != "" {
		return nil
	}
	e := mcp.Entry{Name: "composio", URL: composio.ConnectURL, Headers: map[string]string{"x-consumer-api-key": "${" + composioKey + "}"}}
	_, err := c.addMCP(ctx, e, func() (secrets.Resolver, error) { return c.resolver(octx) })
	if err != nil && !errors.HasCode(err, mcp.ErrConnect) && !errors.HasCode(err, errMCPNextSession) {
		return err
	}
	return c.settings.settle(settleWait)
}

// deleteComposio removes the server at Composio Connect and the key.
func (c *cli) deleteComposio(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, _, ok := c.secretStoreNamed(w, r, octx, composioKey)
		if !ok {
			return
		}
		if !c.removeComposioServer() {
			fail(w, http.StatusConflict, "SETTINGS_FILE", "the Composio server could not be removed from ringil.yaml")
			return
		}
		if err := st.Delete(r.Context(), composioKey); err != nil {
			fail(w, http.StatusInternalServerError, "SETTINGS_SECRETS", describe(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (c *cli) removeComposioServer() bool {
	rec := &statusRecord{header: http.Header{}}
	target := ""
	c.change(rec, func(next *config) *refusal {
		target = composioServerIn(next)
		if target == "" {
			return notFound("no composio server")
		}
		delete(next.MCPServers, target)
		return nil
	}, func(ed *edit) { ed.set("mcp_servers."+target, nil) })
	return rec.status == http.StatusNoContent || rec.status == http.StatusNotFound
}

type statusRecord struct {
	header http.Header
	status int
}

func (s *statusRecord) Header() http.Header         { return s.header }
func (s *statusRecord) Write(p []byte) (int, error) { return len(p), nil }
func (s *statusRecord) WriteHeader(code int)        { s.status = code }

// Remote agents.

// RemoteAgentIO is a remote agent the main agent may ask (A2A).
type RemoteAgentIO struct {
	Name             string            `json:"name,omitempty"`
	URL              string            `json:"url"`
	Headers          map[string]string `json:"headers,omitempty"`
	AllowOtherOrigin bool              `json:"allow_other_origin,omitempty"`
	Disabled         bool              `json:"disabled,omitempty"`
}

func (c *cli) getRemoteAgents(w http.ResponseWriter, _ *http.Request) {
	f := c.conf()
	out := []RemoteAgentIO{}
	for _, name := range sortedKeys(f.Agents) {
		e := f.Agents[name]
		out = append(out, RemoteAgentIO{Name: name, URL: e.URL, Headers: e.Headers, AllowOtherOrigin: e.AllowOtherOrigin, Disabled: e.Disabled})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out, "expose": f.Serve.A2A})
}

// putRemoteAgent reads the remote agent's card before it saves it; it is
// asked from the next start of serve.
func (c *cli) putRemoteAgent(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var in RemoteAgentIO
		if !decodeJSON(w, r, &in) {
			return
		}
		if msg := checkRemoteIn(name, in); msg != "" {
			fail(w, http.StatusBadRequest, "SETTINGS_INPUT", msg)
			return
		}
		if !in.Disabled { // its card is read: one that does not answer is not saved
			if code, err := c.readCard(r.Context(), octx, name, in); err != nil {
				fail(w, code, "SETTINGS_A2A", describe(err))
				return
			}
		}
		c.change(w, func(next *config) *refusal {
			if next.Agents == nil {
				next.Agents = map[string]remoteEntry{}
			}
			next.Agents[name] = remoteEntry{URL: in.URL, Headers: in.Headers, AllowOtherOrigin: in.AllowOtherOrigin, Disabled: in.Disabled}
			return nil
		}, func(ed *edit) {
			at := "agents." + name
			ed.set(at, map[string]interface{}{})
			ed.set(at+".url", in.URL)
			ed.set(at+".headers", headerMap(in.Headers))
			ed.set(at+".allow_other_origin", trueOrNone(in.AllowOtherOrigin))
			ed.set(at+".disabled", trueOrNone(in.Disabled))
		})
	}
}

// checkRemoteIn says what is wrong with a remote agent the app sends, ""
// for nothing.
func checkRemoteIn(name string, in RemoteAgentIO) string {
	if in.Name != "" && in.Name != name {
		return "the name is the path's"
	}
	if !profileName.MatchString(name) {
		return "a remote agent's name is 1-64 letters, digits, - and _"
	}
	if u, err := url.Parse(in.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "a remote agent's url is an http(s) address"
	}
	return badHeaders(in.Headers)
}

// badHeaders says which header is not a reference to a secret, "" for
// none.
func badHeaders(h map[string]string) string {
	for k, v := range h {
		if !headerName.MatchString(k) {
			return "not a header's name: " + strconv.Quote(k)
		}
		if !isReference(v) {
			return "header " + k + ": a header's value is a reference such as ${NAME}, its secret kept with /v1/settings/secrets"
		}
	}
	return ""
}

// headerMap is headers as ringil.yaml's value; none, nil.
func headerMap(h map[string]string) interface{} {
	if len(h) == 0 {
		return nil
	}
	out := map[string]interface{}{}
	for k, v := range h {
		out[k] = v
	}
	return out
}

// readCard reads a remote agent's card, with the status of a failure.
func (c *cli) readCard(ctx context.Context, octx *orpheus.Context, name string, in RemoteAgentIO) (int, error) {
	res, err := c.resolver(octx)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	remote, err := a2a.Connect(ctx, a2a.Remote{Name: name, URL: in.URL, AllowOtherOrigin: in.AllowOtherOrigin, Headers: headerFunc(in.Headers, res)})
	if err != nil {
		return http.StatusBadGateway, errors.Wrap(err, ErrUsage, "its card could not be read")
	}
	_ = remote.Close()
	return 0, nil
}

func (c *cli) deleteRemoteAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c.change(w, func(next *config) *refusal {
		if _, ok := next.Agents[name]; !ok {
			return notFound("no remote agent " + name)
		}
		delete(next.Agents, name)
		return nil
	}, func(ed *edit) { ed.set("agents."+name, nil) })
}

type a2aIn struct {
	Expose bool `json:"expose"`
}

// putA2A offers the main agent to other agents, at /a2a with serve's
// token, from the next start.
func (c *cli) putA2A(w http.ResponseWriter, r *http.Request) {
	var in a2aIn
	if !decodeJSON(w, r, &in) {
		return
	}
	c.change(w, func(next *config) *refusal { next.Serve.A2A = in.Expose; return nil },
		func(ed *edit) { ed.set("serve.a2a", trueOrNone(in.Expose)) })
}

// Restart.

// busyForRestart says what would be cut by a restart now: a workflow
// going (between steps too), an import on its way; "" for nothing.
func (c *cli) busyForRestart() string {
	if c.wfRuns.any() {
		return "a workflow is going: try again when it ends"
	}
	c.imports.mu.Lock()
	defer c.imports.mu.Unlock()
	if c.imports.receiving || c.imports.archive != "" {
		return "an import is on its way"
	}
	return ""
}

// restarting: serve was asked to start again (POST /v1/restart).
var restarting atomic.Bool

// restartServe stops serve to start it again, when no run is going:
// what only a start takes (MCP servers, remote agents, chat apps, the
// address) is then in force.
func (c *cli) restartServe(srv *serve.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c.imports == nil || c.imports.stop == nil {
			fail(w, http.StatusConflict, "SERVE", "this serve cannot start itself again")
			return
		}
		if why := c.busyForRestart(); why != "" {
			fail(w, http.StatusConflict, "SERVE_BUSY", why)
			return
		}
		// Held: no run starts between here and the end (audit 27/11).
		if _, err := srv.Pause(r.Context(), time.Second); err != nil {
			fail(w, http.StatusConflict, "SERVE_BUSY", "a run is going: try again when it ends")
			return
		}
		restarting.Store(true)
		writeJSON(w, http.StatusAccepted, map[string]any{"restarting": true})
		go func() {
			time.Sleep(200 * time.Millisecond) // the answer leaves first
			c.imports.stop()
		}()
	}
}
