// models.go: the models a subscription offers
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/httpcall"
)

// clientVersion is the Codex version the model list is asked for: the
// backend lists only the models a client of that version can use (22/11:
// none below 1.0.0), so a version above any Codex release gets them all.
const clientVersion = "99.0.0"

// Model is one model of the subscription.
type Model struct {
	Name          string // the name to configure (its slug)
	Title         string // the name shown
	Description   string
	ContextWindow int
	// Effort is the model's default reasoning effort; Efforts the ones it
	// takes.
	Effort  string
	Efforts []string
}

// Models lists the models the signed-in subscription offers, in the
// backend's order of preference. base and client as in Config.
func Models(ctx context.Context, s *Session, base string, client *http.Client) ([]Model, error) {
	endpoint, err := endpointURL(base, "/models")
	if err != nil {
		return nil, err
	}
	tok, err := s.Token(ctx)
	if err != nil {
		return nil, signedOut(err)
	}
	resp, err := listModels(ctx, endpoint, tok, client)
	if err == nil && resp.StatusCode == http.StatusUnauthorized { // renewed elsewhere, or revoked
		_ = resp.Body.Close() // #nosec G104 -- asked again below
		if tok, err = s.Renew(ctx, tok); err != nil {
			return nil, signedOut(err)
		}
		resp, err = listModels(ctx, endpoint, tok, client)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }() // #nosec G104 -- read below
	if resp.StatusCode/100 != 2 {
		return nil, httpcall.StatusError(resp)
	}
	return parseModels(resp.Body)
}

func listModels(ctx context.Context, endpoint string, tok Tokens, client *http.Client) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+url.Values{"client_version": {clientVersion}}.Encode(), nil)
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrProviderConfig, "request cannot be built")
	}
	req.Header.Set("Authorization", "Bearer "+tok.Access)
	req.Header.Set("ChatGPT-Account-ID", tok.Account)
	req.Header.Set("originator", originator)
	resp, err := noRedirect(client).Do(req)
	if err != nil {
		return nil, httpcall.NetworkError(ctx, err)
	}
	return resp, nil
}

// parseModels are the listed models, in the backend's order of
// preference; hidden ones (its own) are left out.
func parseModels(r io.Reader) ([]Model, error) {
	var wire struct {
		Models []struct {
			Slug        string `json:"slug"`
			Display     string `json:"display_name"`
			Description string `json:"description"`
			Visibility  string `json:"visibility"`
			Priority    int    `json:"priority"`
			Context     int    `json:"context_window"`
			Effort      string `json:"default_reasoning_level"`
			Efforts     []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 4<<20)).Decode(&wire); err != nil {
		return nil, errors.New(agent.ErrProviderResponse, "the model list is not valid JSON")
	}
	sort.SliceStable(wire.Models, func(i, j int) bool { return wire.Models[i].Priority < wire.Models[j].Priority })
	var out []Model
	for _, m := range wire.Models {
		if m.Slug == "" || m.Visibility != "list" {
			continue
		}
		model := Model{Name: m.Slug, Title: m.Display, Description: m.Description, ContextWindow: max(m.Context, 0), Effort: m.Effort}
		for _, e := range m.Efforts {
			model.Efforts = append(model.Efforts, e.Effort)
		}
		out = append(out, model)
	}
	return out, nil
}
