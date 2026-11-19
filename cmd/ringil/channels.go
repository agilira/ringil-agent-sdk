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
// the environment or the --env-file). The owners (--telegram-owners) talk
// with --profile, else default, and approve; the members (listed, or in
// --telegram-company-group, or sharing a --telegram-member-phones number)
// with --member-profile; with --public-profile, anyone else with it. A
// message from anyone else is noted with its ID.
func (c *cli) startChannels(ctx *orpheus.Context, run context.Context, srv *serve.Server) (func(), error) {
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	token, found, err := secrets.Lookup(run, res, "RINGIL_TELEGRAM_TOKEN")
	if err != nil || !found {
		return func() {}, err
	}
	cfg, err := c.telegramConfig(ctx, run, res, token, srv)
	if err != nil {
		return nil, err
	}
	b, err := channel.NewBridge(cfg)
	if err != nil {
		return nil, err
	}
	stop := background(run, b.Run)
	if len(cfg.Owners) == 0 {
		c.note("telegram connected; no owner yet: write to the bot, then restart with --telegram-owners <your ID>")
	} else {
		c.note("telegram connected")
	}
	return stop, nil
}

// telegramConfig connects the bot and sets up its bridge.
func (c *cli) telegramConfig(ctx *orpheus.Context, run context.Context, res secrets.Resolver, token string, srv *serve.Server) (channel.Config, error) {
	tg, err := telegram.New(run, telegram.Config{Token: token})
	if err != nil {
		return channel.Config{}, errors.Wrap(err, ErrUsage, "telegram: "+err.Error())
	}
	cfg, err := c.bridgeConfig(ctx, res)
	if err != nil {
		return channel.Config{}, err
	}
	cfg.Channel, cfg.Server = tg, srv
	if group := ctx.GetFlagString("telegram-company-group"); group != "" {
		cfg.IsMember = tg.MemberOf(group)
	}
	roles, err := c.loadProfiles(ctx)
	if err != nil {
		return channel.Config{}, err
	}
	for _, name := range []string{cfg.MemberProfile, cfg.PublicProfile} {
		if err := checkChatProfile(roles, name); err != nil {
			return channel.Config{}, err
		}
	}
	return cfg, nil
}

// background runs work until the function returned stops it and waits.
func background(ctx context.Context, work func(context.Context) error) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = work(ctx)
	}()
	return func() { cancel(); <-done }
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
	return channel.Config{Profile: a2aProfile(ctx), Owners: splitList(ctx.GetFlagString("telegram-owners")),
		Members: splitList(ctx.GetFlagString("telegram-members")), MemberProfile: ctx.GetFlagString("member-profile"),
		MemberPhones: splitList(ctx.GetFlagString("telegram-member-phones")), SharedPhones: filepath.Join(filepath.Dir(c.inboxRoot), "telegram-phones.json"),
		PublicProfile: ctx.GetFlagString("public-profile"), MaxPerHour: ctx.GetFlagInt("max-per-hour"),
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
		c.note("telegram: message from user " + clean(e.From) + " not allowed (--telegram-owners or --telegram-members " + clean(e.From) + " to let them in)")
	case channel.Limited:
		c.note("telegram: user " + clean(e.From) + " went past --max-per-hour")
	default:
		if what := failures(e); what != "" && e.Err != nil {
			c.note("telegram: " + what + ": " + clean(oneLine(e.Err.Error(), 300)))
		}
	}
}

// failures says what failed, for the events that carry an error.
func failures(e channel.Event) string {
	return map[channel.EventKind]string{
		channel.MemberCheckFailed: "cannot check whether user " + clean(e.From) + " is in the company group",
		channel.ReceiveFailed:     "cannot receive, retrying",
		channel.FileFailed:        "a file in chat " + clean(e.Chat) + " could not be taken",
		channel.VoiceFailed:       "an answer went as text, not voice",
		channel.SessionsFailed:    "the chats' conversations could not be saved",
		channel.PhonesFailed:      "the phone numbers shared could not be saved",
		channel.SendFailed:        "cannot send to chat " + clean(e.Chat),
	}[e.Kind]
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
