// session.go: a sign-in kept fresh, shared by several processes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

// refreshMargin: the access token is refreshed this long before it
// expires, so no request carries one that dies on the way.
const refreshMargin = 5 * time.Minute

// Session hands out a fresh access token from a Store, refreshing it when
// it nears its end. OpenAI rotates the refresh token at every refresh and
// refuses an old one, so processes sharing a Store must not refresh at
// once: Lock, when set, is held across processes while refreshing, and
// the Store is read again under it, since another process may have just
// refreshed. The Store must therefore read its backing store each time,
// not a cache of it.
type Session struct {
	Store Store
	// Lock takes a lock shared by the processes using Store; FileLock
	// makes one. Nil locks within this process only.
	Lock func(ctx context.Context) (unlock func(), err error)
	// Client and Issuer as in Login.
	Client *http.Client
	Issuer string
	// Warn hears what does not stop the session: refreshed tokens that
	// could not be kept (they are used, and kept at the next call).
	Warn func(error)

	mu      sync.Mutex
	cur     Tokens
	unsaved bool
}

// String hides the tokens the session holds.
func (s *Session) String() string { return "chatgpt.Session" }

// GoString hides them from %#v as well.
func (s *Session) GoString() string { return s.String() }

// Save keeps the tokens of a new sign-in, under the lock: a refresh of
// another process cannot then put the old sign-in back.
func (s *Session) Save(ctx context.Context, t Tokens) error {
	if err := t.check(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.Store.Save(ctx, t); err != nil {
		return err
	}
	s.cur, s.unsaved = t, false
	return nil
}

func (s *Session) login() Login { return Login{Client: s.Client, Issuer: s.Issuer} }

func (s *Session) issuerURL() (string, error) {
	u, err := s.login().issuerURL()
	if err != nil {
		return "", errors.Wrap(err, ErrTokens, err.Error())
	}
	return u, nil
}

// Token is a usable access token, refreshed when near its end.
func (s *Session) Token(ctx context.Context) (Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur.Access != "" && !nearEnd(s.cur) {
		s.keepUnsaved(ctx)
		return s.cur, nil
	}
	return s.renew(ctx, "")
}

// keepUnsaved tries again to keep refreshed tokens a save missed: the
// store's refresh token is spent, only these work.
func (s *Session) keepUnsaved(ctx context.Context) {
	if !s.unsaved {
		return
	}
	if unlock, err := s.lock(ctx); err == nil {
		if s.Store.Save(ctx, s.cur) == nil {
			s.unsaved = false
		}
		unlock()
	}
}

// Renew is called when the provider refused stale (401): a newer token
// kept by another process is taken, or the token is refreshed.
func (s *Session) Renew(ctx context.Context, stale Tokens) (Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur.Access != "" && s.cur.Access != stale.Access {
		return s.cur, nil // renewed meanwhile by another call
	}
	return s.renew(ctx, stale.Access)
}

func nearEnd(t Tokens) bool { return !t.Expires.IsZero() && time.Until(t.Expires) < refreshMargin }

// renew reads the Store under the lock and refreshes unless what it holds
// is fresh and not the token refused.
func (s *Session) renew(ctx context.Context, refused string) (Tokens, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return Tokens{}, err
	}
	defer unlock()
	if s.unsaved {
		return s.refreshAndKeep(ctx, s.cur)
	}
	kept, ok, err := s.Store.Load(ctx)
	switch {
	case err != nil:
		return Tokens{}, err
	case !ok:
		return Tokens{}, errors.New(ErrSignedOut, "not signed in to ChatGPT: sign in first")
	case kept.Access != "" && kept.Access != refused && !nearEnd(kept):
		if err := kept.check(); err != nil {
			return Tokens{}, err
		}
		s.cur = kept
		return kept, nil
	}
	return s.refreshAndKeep(ctx, kept)
}

// refreshAndKeep refreshes and keeps the new tokens. A save that fails
// does not drop them: OpenAI rotated the refresh token, so they are the
// only ones that work; they are used, and kept at the next call.
func (s *Session) refreshAndKeep(ctx context.Context, kept Tokens) (Tokens, error) {
	fresh, err := s.refresh(ctx, kept)
	if err != nil {
		return Tokens{}, err
	}
	s.cur, s.unsaved = fresh, false
	if err := s.Store.Save(ctx, fresh); err != nil {
		s.unsaved = true
		s.warn(errors.Wrap(err, ErrTokens, "the refreshed ChatGPT sign-in could not be kept yet"))
	}
	return fresh, nil
}

func (s *Session) warn(err error) {
	if s.Warn != nil {
		s.Warn(err)
	}
}

func (s *Session) lock(ctx context.Context) (func(), error) {
	if s.Lock == nil {
		return func() {}, nil
	}
	unlock, err := s.Lock(ctx)
	if err != nil {
		return nil, errors.Wrap(err, ErrTokens, "cannot lock the sign-in to refresh it")
	}
	return unlock, nil
}

// refresh trades the refresh token for new tokens. A refusal means the
// sign-in is over (expired, revoked, or rotated by a process that did
// not share the lock): the user signs in again.
func (s *Session) refresh(ctx context.Context, prev Tokens) (Tokens, error) {
	if prev.Refresh == "" {
		return Tokens{}, errors.New(ErrSignedOut, "the sign-in cannot be refreshed: sign in again")
	}
	issuer, err := s.issuerURL()
	if err != nil {
		return Tokens{}, err
	}
	body, _ := json.Marshal(map[string]string{"grant_type": "refresh_token", "client_id": clientID, "refresh_token": prev.Refresh}) // #nosec G117 -- sent to the token endpoint only
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/oauth/token", bytes.NewReader(body))
	if err != nil {
		return Tokens{}, errors.Wrap(err, ErrTokens, "the refresh cannot be built")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.login().client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Tokens{}, ctx.Err()
		}
		return Tokens{}, errors.Wrap(err, ErrTokens, "OpenAI's sign-in did not answer the refresh").AsRetryable()
	}
	defer func() { _ = resp.Body.Close() }() // #nosec G104 -- read below
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return Tokens{}, refreshRefused(resp.StatusCode, data)
	}
	var got tokenAnswer
	if json.Unmarshal(data, &got) != nil {
		return Tokens{}, errors.New(ErrTokens, "OpenAI's refresh answered in an unknown form")
	}
	fresh := got.tokens(prev)
	if err := fresh.check(); err != nil {
		return Tokens{}, err
	}
	return fresh, nil
}

// refreshRefused names why, from the error codes OpenAI gives, never
// from the body's text.
func refreshRefused(status int, body []byte) error {
	for code, why := range map[string]string{ // #nosec G101 -- OAuth error codes, not credentials
		"refresh_token_expired":     "the sign-in expired",
		"refresh_token_reused":      "the sign-in was refreshed elsewhere (another program with the same sign-in)",
		"refresh_token_invalidated": "the sign-in was revoked",
	} {
		if bytes.Contains(body, []byte(code)) {
			return errors.New(ErrSignedOut, why+": sign in again")
		}
	}
	if status == http.StatusBadRequest || status == http.StatusUnauthorized {
		return errors.New(ErrSignedOut, "OpenAI refused the refresh ("+strconv.Itoa(status)+"): sign in again")
	}
	return errors.New(ErrTokens, "OpenAI's refresh failed ("+strconv.Itoa(status)+")").AsRetryable()
}

// Logout revokes the sign-in at OpenAI and forgets it here, under the
// lock (a refresh of another process cannot put it back). It is
// forgotten even when the revocation fails, or the kept sign-in is
// damaged: unrevoked says why OpenAI was not told, nil when it was.
func (s *Session) Logout(ctx context.Context) (unrevoked, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	unrevoked, err = s.revokeActive(ctx)
	if err != nil {
		return nil, err
	}
	s.cur, s.unsaved = Tokens{}, false
	return unrevoked, s.Store.Delete(ctx)
}

func (s *Session) revokeActive(ctx context.Context) (unrevoked, err error) {
	if s.unsaved && s.cur.Refresh != "" {
		return s.revoke(ctx, s.cur.Refresh), nil
	}
	kept, ok, err := s.Store.Load(ctx)
	switch {
	case errors.HasCode(err, ErrTokens):
		return errors.New(ErrTokens, "the kept sign-in was damaged"), nil
	case err != nil:
		return nil, err
	case !ok:
		return nil, errors.New(ErrSignedOut, "not signed in to ChatGPT")
	default:
		return s.revoke(ctx, kept.Refresh), nil
	}
}

func (s *Session) revoke(ctx context.Context, refresh string) error {
	if refresh == "" {
		return errors.New(ErrTokens, "the sign-in had nothing to revoke")
	}
	issuer, err := s.issuerURL()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"token": refresh, "token_type_hint": "refresh_token", "client_id": clientID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/oauth/revoke", bytes.NewReader(body))
	if err != nil {
		return errors.Wrap(err, ErrTokens, "the revocation cannot be built")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.login().client().Do(req)
	if err != nil {
		return errors.Wrap(err, ErrTokens, "OpenAI could not be reached")
	}
	_ = resp.Body.Close() // #nosec G104 -- only the status counts
	if resp.StatusCode/100 != 2 {
		return errors.New(ErrTokens, "OpenAI refused the revocation ("+strconv.Itoa(resp.StatusCode)+")")
	}
	return nil
}

// FileLock is a Session.Lock across processes: an exclusive lock on the
// file name in dir, a private directory of the user's.
func FileLock(dir, name string) func(ctx context.Context) (func(), error) {
	return func(ctx context.Context) (func(), error) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return nil, err
		}
		f, err := lockfile.Open(root, name)
		_ = root.Close() // #nosec G104 -- the file stays open
		if err != nil {
			return nil, err
		}
		if err := lockfile.Lock(ctx, f); err != nil {
			_ = f.Close() // #nosec G104 -- not locked
			return nil, err
		}
		return func() {
			_ = lockfile.Unlock(f) // #nosec G104 -- closing releases it too
			_ = f.Close()          // #nosec G104 -- the lock is released
		}, nil
	}
}
