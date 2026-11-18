// a2a.go: remote agents as tools
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package a2a lets an agent hand work to remote agents that speak the
// Agent2Agent protocol, through the official Go SDK. Each remote agent
// becomes one tool: the model sends it a message and gets its answer,
// waiting while the remote task runs. It is a module of its own, so the
// agent core keeps no dependency on it.
//
// The remote agent is described by its agent card, fetched from the
// agent's URL. Credentials come from the host on every request and go
// only to the card's origin: a card naming an endpoint on another origin
// is refused unless the host allows it.
package a2a

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	proto "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
)

// Error codes.
const (
	ErrConfig  errors.ErrorCode = "A2A_CONFIG"
	ErrConnect errors.ErrorCode = "A2A_CONNECT"
	ErrCall    errors.ErrorCode = "A2A_CALL"
)

// Remote describes a remote agent.
type Remote struct {
	// Name of the tool the model calls; letters, digits, _ and -.
	Name string
	// URL of the agent. A bare origin (https://agent.example) has its
	// card read from /.well-known/agent-card.json; a URL with a path is
	// read as the card itself.
	URL string
	// AllowHTTP permits http to hosts other than loopback.
	AllowHTTP bool
	// AllowOtherOrigin accepts a card whose endpoint is on another origin
	// than URL; credentials then go there too.
	AllowOtherOrigin bool
	// Headers for every request, such as an API key from the host's
	// secrets.
	Headers safehttp.Headers
	// HTTPClient is the base client.
	HTTPClient *http.Client
	// Timeout bounds one call, the remote task included; default 10
	// minutes. Poll is the interval between task checks; default 1s.
	Timeout time.Duration
	Poll    time.Duration
	// MaxOutput is how much of an answer the model sees; default 256 KiB.
	MaxOutput int
}

var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (r Remote) validate() error {
	if !toolName.MatchString(r.Name) {
		return errors.New(ErrConfig, "remote agent names are 1-64 letters, digits, _ or -")
	}
	if err := safehttp.CheckURL(r.URL, r.AllowHTTP); err != nil {
		return errors.Wrap(err, ErrConfig, "remote agent "+r.Name+": "+err.Error())
	}
	return nil
}

// Agent is a connection to one remote agent.
type Agent struct {
	remote Remote
	client *a2aclient.Client
	card   *proto.AgentCard
	mu     sync.Mutex
	seen   map[string]bool // context and task IDs the remote gave us
}

// Connect reads the remote agent's card and prepares the client.
func Connect(ctx context.Context, r Remote) (*Agent, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	httpc := safehttp.Client(r.HTTPClient, r.Headers, 16<<20, false) // this client never streams
	resolver := &agentcard.Resolver{Client: httpc}
	card, err := resolver.Resolve(ctx, r.URL)
	if err != nil {
		return nil, errors.Wrap(err, ErrConnect, "cannot read the card of remote agent "+r.Name+": "+err.Error())
	}
	if err := checkEndpoints(r, card); err != nil {
		return nil, err
	}
	factory := a2aclient.NewFactory(a2aclient.WithJSONRPCTransport(httpc), a2aclient.WithRESTTransport(httpc))
	client, err := factory.CreateFromCard(ctx, card)
	if err != nil {
		return nil, errors.Wrap(err, ErrConnect, "cannot reach remote agent "+r.Name+": "+err.Error())
	}
	return &Agent{remote: r, client: client, card: card}, nil
}

// checkEndpoints keeps credentials on the origin the host chose: a card
// may name any URL as its endpoint.
func checkEndpoints(r Remote, card *proto.AgentCard) error {
	if len(card.SupportedInterfaces) == 0 {
		return errors.New(ErrConnect, "remote agent "+r.Name+" offers no endpoint")
	}
	for _, i := range card.SupportedInterfaces {
		if i == nil || safehttp.CheckURL(i.URL, r.AllowHTTP) != nil {
			return errors.New(ErrConnect, "remote agent "+r.Name+" offers an endpoint that is not https")
		}
		if !r.AllowOtherOrigin && !safehttp.SameOrigin(i.URL, r.URL) {
			return errors.New(ErrConnect, "remote agent "+r.Name+" names an endpoint on another origin; allow it explicitly if expected")
		}
		if loopbackURL(i.URL) && !loopbackURL(r.URL) {
			return errors.New(ErrConnect, "remote agent "+r.Name+" names an endpoint on this machine")
		}
	}
	return nil
}

func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (safehttp.Loopback(u.Hostname()) || strings.EqualFold(u.Hostname(), "localhost"))
}

// Card is the remote agent's card, as it described itself.
func (a *Agent) Card() *proto.AgentCard { return a.card }

// Close releases the client.
func (a *Agent) Close() error { return a.client.Destroy() }

// description presents the agent to the model: its name, what it says it
// does and its skills, bounded.
func (a *Agent) description() string {
	var b strings.Builder
	b.WriteString("Ask the remote agent " + a.card.Name + ". " + a.card.Description)
	for i, s := range a.card.Skills {
		if i == 10 {
			b.WriteString("\n- ...")
			break
		}
		b.WriteString("\n- " + s.Name + ": " + s.Description)
	}
	b.WriteString("\nIt answers in its own words; pass context_id from its last answer to continue the same conversation, task_id to answer its question.")
	return clip(b.String(), 4096)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + " [...]"
}
