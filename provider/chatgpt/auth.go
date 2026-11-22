// auth.go: a ChatGPT sign-in's tokens, and where they are kept
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

// Error codes of the sign-in.
const (
	// ErrSignIn: the sign-in did not complete (refused, timed out, cut).
	ErrSignIn errors.ErrorCode = "CHATGPT_SIGN_IN"
	// ErrSignedOut: there is no usable sign-in; sign in again.
	ErrSignedOut errors.ErrorCode = "CHATGPT_SIGNED_OUT"
	// ErrTokens: the tokens could not be kept or read back.
	ErrTokens errors.ErrorCode = "CHATGPT_TOKENS" // #nosec G101 -- an error code
)

// Tokens are what a sign-in gives: whoever holds them uses the user's
// subscription, so they belong in a secrets store, never in a file of
// settings or a log.
type Tokens struct {
	Access  string `json:"access_token,omitempty"`
	Refresh string `json:"refresh_token"`
	// Account is the ChatGPT account the requests are made for.
	Account string `json:"account_id"`
	// Expires is when Access stops working (from its own claims).
	Expires time.Time `json:"expires,omitzero"`
}

// String hides the tokens, should they ever be printed.
func (t Tokens) String() string { return "chatgpt.Tokens{account " + t.Account + "}" }

// GoString hides the tokens from %#v as well.
func (t Tokens) GoString() string { return t.String() }

// check refuses tokens a request could not use.
func (t Tokens) check() error {
	switch {
	case t.Access == "" || t.Account == "":
		return errors.New(ErrSignedOut, "the sign-in gave no access token or account")
	case !headerSafe(t.Access) || !headerSafe(t.Account) || !utf8.ValidString(t.Refresh):
		return errors.New(ErrTokens, "the tokens hold characters a request cannot carry")
	}
	return nil
}

func headerSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// Store keeps the tokens between runs. Load reports false when there are
// none. Several processes may share one (a server and a terminal), so a
// Session reads it again before refreshing.
type Store interface {
	Load(ctx context.Context) (Tokens, bool, error)
	Save(ctx context.Context, t Tokens) error
	Delete(ctx context.Context) error
}

// SecretStore keeps the tokens in a secrets store (a keyring, Vault, an
// encrypted file) as two secrets: name, the refresh token with the
// account and the expiry, and name_ACCESS, the access token. Apart, each
// fits the keyrings' size limits (Windows: 2560 bytes; together they
// passed it). An access token that is not the one name expects (a save
// cut between the two) is not used: the session refreshes.
//
// A Session reads the store again under its lock to see another
// process's refresh: s must not answer from a cache (store.Fresh).
func SecretStore(s secrets.Store, name string) Store { return secretStore{s: s, name: name} }

type secretStore struct {
	s    secrets.Store
	name string
}

func (s secretStore) access() string { return s.name + "_ACCESS" }

func (s secretStore) Load(ctx context.Context) (Tokens, bool, error) {
	v, ok, err := secrets.Lookup(ctx, s.s, s.name)
	if err != nil || !ok {
		return Tokens{}, false, err
	}
	var t Tokens
	if err := json.Unmarshal([]byte(v), &t); err != nil {
		return Tokens{}, false, errors.New(ErrTokens, "the kept sign-in is damaged: sign in again")
	}
	access, ok, err := secrets.Lookup(ctx, s.s, s.access())
	if err != nil {
		return Tokens{}, false, err
	}
	if ok && expiryOf(access).Unix() == t.Expires.Unix() {
		t.Access = access
	}
	return t, true, nil
}

// Save writes the access token first: a save cut in between leaves the
// old record, which does not expect the new token, and the session
// refreshes with the old refresh token, still the valid one.
func (s secretStore) Save(ctx context.Context, t Tokens) error {
	if err := s.s.Set(ctx, s.access(), t.Access); err != nil {
		return err
	}
	t.Access = ""
	data, err := json.Marshal(t) // #nosec G117 -- the secret store is where the tokens belong
	if err != nil {
		return errors.Wrap(err, ErrTokens, "the tokens do not encode")
	}
	return s.s.Set(ctx, s.name, string(data))
}

// Delete removes the record first: what is left is never usable.
func (s secretStore) Delete(ctx context.Context) error {
	if err := s.s.Delete(ctx, s.name); err != nil {
		return err
	}
	return s.s.Delete(ctx, s.access())
}

// claims reads a JWT's payload without checking its signature: the
// tokens come from the token endpoint over TLS, and only the account and
// the expiry are taken from them, to address requests and refresh early.
func claims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) > 64<<10 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var c map[string]any
	if json.Unmarshal(payload, &c) != nil {
		return nil
	}
	return c
}

// authClaim is the claim where OpenAI puts the account.
const authClaim = "https://api.openai.com/auth"

// accountOf is the ChatGPT account of the first token that names one.
func accountOf(tokens ...string) string {
	for _, tok := range tokens {
		if auth, ok := claims(tok)[authClaim].(map[string]any); ok {
			if id, ok := auth["chatgpt_account_id"].(string); ok && id != "" {
				return id
			}
		}
	}
	return ""
}

// expiryOf is when token expires, or zero when it does not say.
func expiryOf(token string) time.Time {
	if exp, ok := claims(token)["exp"].(float64); ok && exp > 0 && exp < 1e11 { // before the year 5000
		return time.Unix(int64(exp), 0)
	}
	return time.Time{}
}
