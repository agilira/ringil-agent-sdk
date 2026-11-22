// onboard_chatgpt.go: a ChatGPT subscription in the guided setup
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/agilira/ringil-agent-sdk/provider/chatgpt"
)

// askCredential is the provider's credential: a sign-in for a ChatGPT
// subscription, else a key.
func (s *setup) askCredential() error {
	if s.provider.kind == providerChatGPT {
		return s.signInChatGPT()
	}
	return s.askKey()
}

// signInChatGPT signs in with the user's subscription in place of a key:
// in the browser here, or by a code typed on another device when there
// is no browser (a server, SSH). The sign-in goes to the store chosen.
func (s *setup) signInChatGPT() error {
	dir, err := s.c.configDir()
	if err != nil {
		return err
	}
	s.session = &chatgpt.Session{
		Store:  chatgpt.SecretStore(s.keys, chatgptLogin),
		Lock:   chatgpt.FileLock(filepath.Join(dir, "ringil"), "chatgpt.lock"),
		Issuer: s.c.chatgptIssuer,
	}
	here, err := s.p.Confirm("Is there a browser on this machine, to sign in to ChatGPT?", true)
	if err != nil {
		return err
	}
	login := chatgpt.Login{Show: s.c.showLogin, Issuer: s.c.chatgptIssuer, Port: s.c.chatgptPort}
	signIn := login.Device
	if here {
		signIn = login.Browser
	}
	tok, err := signIn(s.ctx)
	if err != nil {
		return err
	}
	if err := s.session.Save(s.ctx, tok); err != nil {
		return err
	}
	return s.c.line("  signed in to ChatGPT")
}

// chatgptModels are the subscription's models, in its order.
func (s *setup) chatgptModels() ([]string, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	models, err := chatgpt.Models(ctx, s.session, s.c.chatgptBase, nil)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Name)
	}
	return names, nil
}
