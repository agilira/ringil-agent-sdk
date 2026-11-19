// channels.go: chat apps talking with the served agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/channel/telegram"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/serve"
)

// startChannels connects Telegram when RINGIL_TELEGRAM_TOKEN is set (in
// the environment or the --env-file): its chats talk with the profile of
// --profile, else default. Only the user IDs of --telegram-allow are
// heard; a message from anyone else is noted with its ID.
func (c *cli) startChannels(ctx *orpheus.Context, run context.Context, srv *serve.Server) (func(), error) {
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	token, found, err := secrets.Lookup(run, res, "RINGIL_TELEGRAM_TOKEN")
	if err != nil || !found {
		return func() {}, err
	}
	tg, err := telegram.New(run, telegram.Config{Token: token})
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "telegram: "+err.Error())
	}
	cfg, err := c.bridgeConfig(ctx, res)
	if err != nil {
		return nil, err
	}
	cfg.Channel, cfg.Server = tg, srv
	b, err := channel.NewBridge(cfg)
	if err != nil {
		return nil, err
	}
	run, cancel := context.WithCancel(run)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.Run(run)
	}()
	if len(cfg.Allow) == 0 {
		c.note("telegram connected; nobody is allowed yet: write to the bot, then restart with --telegram-allow <your ID>")
	} else {
		c.note("telegram connected")
	}
	return func() { cancel(); <-done }, nil
}

// bridgeConfig is what a chat app's bridge does, from the flags and
// settings: who is heard, voice in and out, where documents go.
func (c *cli) bridgeConfig(ctx *orpheus.Context, res secrets.Resolver) (channel.Config, error) {
	replies := ctx.GetFlagString("replies")
	if replies != "voice" && replies != "text" {
		return channel.Config{}, errors.New(ErrUsage, "--replies is voice or text")
	}
	ear, err := c.transcriber(ctx, res)
	if err != nil {
		return channel.Config{}, err
	}
	mouth, err := c.synthesizer(ctx, res)
	if err != nil {
		return channel.Config{}, err
	}
	idle, err := newAfter(ctx.GetFlagString("new-after"))
	if err != nil {
		return channel.Config{}, err
	}
	if c.inboxRoot, err = c.inboxDir(); err != nil {
		return channel.Config{}, err
	}
	return channel.Config{Profile: a2aProfile(ctx), Allow: splitList(ctx.GetFlagString("telegram-allow")),
		Transcriber: ear, Synthesizer: mouth, VoiceReplies: replies == "voice", Inbox: c.inboxRoot,
		Sessions: filepath.Join(filepath.Dir(c.inboxRoot), "telegram-sessions.json"), IdleReset: idle, OnEvent: c.channelEvent}, nil
}

// newAfter reads --new-after: a duration (at least a minute), or never.
func newAfter(s string) (time.Duration, error) {
	if s == "never" {
		return -1, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Minute {
		return 0, errors.New(ErrUsage, "--new-after is a duration of a minute or more (such as 24h or 90m), or never")
	}
	return d, nil
}

func (c *cli) channelEvent(e channel.Event) {
	switch e.Kind {
	case channel.Refused:
		c.note("telegram: message from user " + clean(e.From) + " not allowed (to allow it: --telegram-allow " + clean(e.From) + ")")
	case channel.ReceiveFailed:
		c.note("telegram: cannot receive, retrying: " + clean(oneLine(e.Err.Error(), 300)))
	case channel.FileFailed:
		c.note("telegram: a file in chat " + clean(e.Chat) + " could not be taken: " + clean(oneLine(e.Err.Error(), 300)))
	case channel.VoiceFailed:
		c.note("telegram: an answer went as text, not voice: " + clean(oneLine(e.Err.Error(), 300)))
	case channel.SessionsFailed:
		c.note("telegram: the chats' conversations could not be saved: " + clean(oneLine(e.Err.Error(), 300)))
	case channel.SendFailed:
		c.note("telegram: cannot send to chat " + clean(e.Chat) + ": " + clean(oneLine(e.Err.Error(), 300)))
	}
}

// inboxDir is where the documents sent in the chats are kept, a
// directory per conversation: <config>/ringil/inbox, private to the user.
func (c *cli) inboxDir() (string, error) {
	base, err := c.configDir()
	if err != nil {
		return "", errors.Wrap(err, ErrUsage, "no user config directory for the documents sent in the chats")
	}
	dir := filepath.Join(base, "ringil", "inbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errors.Wrap(err, ErrUsage, "cannot create "+dir)
	}
	return dir, nil
}

// transcriber hears voice messages with RINGIL_TRANSCRIBE_MODEL; nil
// without one. Its endpoint and key: see audioConfig.
func (c *cli) transcriber(ctx *orpheus.Context, res secrets.Resolver) (agent.Transcriber, error) {
	cfg, err := c.audioConfig(ctx, res, "RINGIL_TRANSCRIBE")
	if err != nil || cfg.Model == "" {
		return nil, err
	}
	return openai.NewTranscriber(cfg)
}

// synthesizer speaks answers with RINGIL_SPEECH_MODEL in the voice
// RINGIL_SPEECH_VOICE; nil without a model.
func (c *cli) synthesizer(ctx *orpheus.Context, res secrets.Resolver) (agent.Synthesizer, error) {
	cfg, err := c.audioConfig(ctx, res, "RINGIL_SPEECH")
	if err != nil || cfg.Model == "" {
		return nil, err
	}
	voice, _, err := secrets.Lookup(ctx.Context(), res, "RINGIL_SPEECH_VOICE")
	if err != nil {
		return nil, err
	}
	return openai.NewSynthesizer(cfg, voice)
}

// audioConfig is an audio model's endpoint: <prefix>_MODEL at
// <prefix>_BASE_URL, else at the chat model's endpoint. The main key goes
// only to the main endpoint: another one gets <prefix>_API_KEY, or no
// key. No model, no config.
func (c *cli) audioConfig(ctx *orpheus.Context, res secrets.Resolver, prefix string) (openai.Config, error) {
	model, _, err := secrets.Lookup(ctx.Context(), res, prefix+"_MODEL")
	if err != nil || model == "" {
		return openai.Config{}, err
	}
	main, err := c.setting(ctx, res, "base", "RINGIL_BASE_URL")
	if err != nil {
		return openai.Config{}, err
	}
	base, _, err := secrets.Lookup(ctx.Context(), res, prefix+"_BASE_URL")
	if err != nil {
		return openai.Config{}, err
	}
	keyName := ctx.GetGlobalFlagString("key-env")
	switch {
	case base != "" && base != main:
		keyName = prefix + "_API_KEY"
	case main == "":
		return openai.Config{}, errors.New(ErrUsage, prefix+"_MODEL needs "+prefix+"_BASE_URL, or RINGIL_BASE_URL")
	default:
		base = main
	}
	if err := validKeyName(keyName); err != nil {
		return openai.Config{}, err
	}
	return openai.Config{BaseURL: base, Model: model, KeyFunc: keyFunc(res, keyName), AllowHTTP: ctx.GetGlobalFlagBool("allow-http")}, nil
}

// inboxOf is the documents directory of a conversation's chat, when it
// has one: a run is granted its own conversation's documents only.
func (c *cli) inboxOf(conversation string) string {
	if c.inboxRoot == "" || conversation == "" || agent.ValidateSessionID(conversation) != nil {
		return ""
	}
	dir := filepath.Join(c.inboxRoot, conversation)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}
