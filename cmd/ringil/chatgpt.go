// chatgpt.go: models of a ChatGPT subscription
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/chatgpt"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/store"
)

const (
	providerChatGPT = "chatgpt"
	// chatgptLogin is the secret the sign-in's tokens are kept in.
	chatgptLogin = "RINGIL_CHATGPT_LOGIN"
)

// chatgptSession is the sign-in, kept in the secrets store and shared by
// every ringil process of the user: the lock in the config directory
// makes them refresh it one at a time.
func (c *cli) chatgptSession(base secrets.Resolver) (*chatgpt.Session, error) {
	c.signIns.mu.Lock()
	defer c.signIns.mu.Unlock()
	if c.signIns.chatgpt != nil {
		return c.signIns.chatgpt, nil
	}
	if c.conf().Secrets.Store == "" {
		return nil, errors.New(ErrUsage, "a ChatGPT sign-in is kept in a secrets store: set one up with `ringil setup`")
	}
	dir, err := c.configDir()
	if err != nil {
		return nil, err
	}
	c.signIns.chatgpt = &chatgpt.Session{
		Store:  keptLogin{c: c, base: base},
		Lock:   chatgpt.FileLock(filepath.Join(dir, "ringil"), "chatgpt.lock"),
		Issuer: c.chatgptIssuer,
		Warn:   func(err error) { c.note(describe(err)) },
	}
	return c.signIns.chatgpt, nil
}

// keptLogin keeps the sign-in in the configured store, asked for at each
// use: the cache of stores may close one (each command closes them when
// it ends) and open it again.
type keptLogin struct {
	c    *cli
	base secrets.Resolver
}

func (k keptLogin) store(ctx context.Context) (chatgpt.Store, error) {
	s, err := k.c.keyStore(ctx, k.base)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New(ErrUsage, "no secrets store for the ChatGPT sign-in: set one up with `ringil setup`")
	}
	return chatgpt.SecretStore(store.Fresh(s), chatgptLogin), nil // read under the lock: never from the cache
}

func (k keptLogin) Load(ctx context.Context) (chatgpt.Tokens, bool, error) {
	s, err := k.store(ctx)
	if err != nil {
		return chatgpt.Tokens{}, false, err
	}
	return s.Load(ctx)
}

func (k keptLogin) Save(ctx context.Context, t chatgpt.Tokens) error {
	s, err := k.store(ctx)
	if err != nil {
		return err
	}
	return s.Save(ctx, t)
}

func (k keptLogin) Delete(ctx context.Context) error {
	s, err := k.store(ctx)
	if err != nil {
		return err
	}
	return s.Delete(ctx)
}

// signIns are the sign-ins of this process, shared with the server's runs.
type signIns struct {
	mu      sync.Mutex
	chatgpt *chatgpt.Session
}

// newChatGPT is the provider for a model of the subscription; base, when
// set, replaces the ChatGPT backend.
func (c *cli) newChatGPT(ctx *orpheus.Context, model, base string) (agent.Provider, error) {
	res, err := c.baseResolver(ctx)
	if err != nil {
		return nil, err
	}
	s, err := c.chatgptSession(res)
	if err != nil {
		return nil, err
	}
	return chatgpt.New(chatgpt.Config{Model: model, Session: s, BaseURL: cmpOr(base, c.chatgptBase)})
}

// cmdLogin signs in with a ChatGPT subscription: in the browser, or with
// --device by a code typed on any device (a server, an SSH session).
func (c *cli) cmdLogin(ctx *orpheus.Context) error {
	if err := oneVendor(ctx); err != nil {
		return err
	}
	base, err := c.baseResolver(ctx)
	if err != nil {
		return err
	}
	s, err := c.chatgptSession(base)
	if err != nil {
		return err
	}
	login := chatgpt.Login{Show: c.showLogin, Issuer: c.chatgptIssuer, Port: c.chatgptPort}
	signIn := login.Browser
	if ctx.GetFlagBool("device") {
		signIn = login.Device
	}
	tok, err := signIn(ctx.Context())
	if err != nil {
		return err
	}
	if err := s.Save(ctx.Context(), tok); err != nil {
		return err
	}
	return c.line("signed in to ChatGPT; the sign-in is kept in the " + c.conf().Secrets.Store + " store")
}

func (c *cli) showLogin(p chatgpt.Prompt) error {
	if p.Code == "" {
		return c.line("Sign in to ChatGPT: open this page in your browser\n  " + p.URL)
	}
	return c.line("Sign in to ChatGPT: open this page on any device\n  " + p.URL +
		"\nand type this code (it lasts 15 minutes)\n  " + p.Code +
		"\nOnly if you started this sign-in: if someone gave you the code, do not type it.")
}

// cmdLogout revokes the sign-in at OpenAI and forgets it here.
func (c *cli) cmdLogout(ctx *orpheus.Context) error {
	if err := oneVendor(ctx); err != nil {
		return err
	}
	base, err := c.baseResolver(ctx)
	if err != nil {
		return err
	}
	s, err := c.chatgptSession(base)
	if err != nil {
		return err
	}
	unrevoked, err := s.Logout(ctx.Context())
	if err != nil {
		return err
	}
	if unrevoked != nil {
		return c.line("signed out here, but not at OpenAI (" + describe(unrevoked) + "): revoke ringil in ChatGPT's settings, or the sign-in ends by itself")
	}
	return c.line("signed out of ChatGPT")
}

func oneVendor(ctx *orpheus.Context) error {
	if args := ctx.Positional(); len(args) != 1 || args[0] != providerChatGPT {
		return errors.New(ErrUsage, "name the account: chatgpt")
	}
	return nil
}
