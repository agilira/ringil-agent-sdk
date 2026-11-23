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
// the environment or the --env-file). The owners (serve.telegram.owners in ringil.yaml) talk
// with --profile, else default, and approve; the members (listed, or in
// serve.telegram.company_group, or sharing a serve.telegram.member_phones number)
// with serve.member_profile; with serve.public_profile, anyone else with it. A
// message from anyone else is noted with its ID.
func (c *cli) startChannels(ctx *orpheus.Context, run context.Context, srv *serve.Server) (func(), error) {
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	token, found, err := secrets.Lookup(run, storeOnly(res), "RINGIL_TELEGRAM_TOKEN") // a planted token would start someone else's bot
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
	c.bridge.Store(b)
	stop := background(run, b.Run)
	if len(cfg.Owners) == 0 {
		c.note("telegram connected; no owner yet: write to the bot, then add your ID to serve.telegram.owners in ringil.yaml")
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
	if group := c.conf().Serve.Telegram.CompanyGroup; group != "" {
		cfg.IsMember = tg.MemberOf(group)
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
	sc := c.conf().Serve
	ear, err := c.transcriber(ctx, res)
	if err != nil {
		return channel.Config{}, err
	}
	mouth, err := c.synthesizer(ctx, res)
	if err != nil {
		return channel.Config{}, err
	}
	idle, err := newAfter(sc.newAfter())
	if err != nil {
		return channel.Config{}, err
	}
	if c.inboxRoot, err = c.inboxDir(); err != nil {
		return channel.Config{}, err
	}
	who := sc.people(c.flagBool(ctx, "no-approvals"))
	return channel.Config{Profile: a2aProfile(ctx), Owners: who.Owners,
		Members: who.Members, MemberProfile: who.MemberProfile,
		MemberPhones: who.MemberPhones, SharedPhones: filepath.Join(filepath.Dir(c.inboxRoot), "telegram-phones.json"),
		PublicProfile: who.PublicProfile, MaxPerHour: who.MaxPerHour,
		Transcriber: ear, Synthesizer: mouth, VoiceReplies: sc.replies() == "voice", Inbox: c.inboxRoot,
		Sessions: filepath.Join(filepath.Dir(c.inboxRoot), "telegram-sessions.json"), IdleReset: idle, OnEvent: c.channelEvent, Automations: c.automationsText,
		Listens: c.listens, Hear: c.hear,
		Language: sc.language(), NoApprovals: who.NoApprovals}, nil
}

// newAfter reads serve.new_after: a duration (at least a minute), or never.
func newAfter(s string) (time.Duration, error) {
	if s == "never" {
		return -1, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Minute {
		return 0, errors.New(ErrUsage, "serve.new_after is a duration of a minute or more (such as 24h or 90m), or never")
	}
	return d, nil
}

func (c *cli) channelEvent(e channel.Event) {
	switch e.Kind {
	case channel.Refused:
		c.note("telegram: message from user " + clean(e.From) + " not allowed (add " + clean(e.From) + " to serve.telegram.owners or members in ringil.yaml to let them in)")
	case channel.Limited:
		c.note("telegram: user " + clean(e.From) + " went past serve.max_per_hour")
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
	cfg, err := c.audioConfig(ctx, res, "RINGIL_TRANSCRIBE", c.conf().Transcribe)
	if err != nil || cfg.Model == "" {
		return nil, err
	}
	return openai.NewTranscriber(cfg)
}

// synthesizer speaks answers with RINGIL_SPEECH_MODEL in the voice
// RINGIL_SPEECH_VOICE; nil without a model.
func (c *cli) synthesizer(ctx *orpheus.Context, res secrets.Resolver) (agent.Synthesizer, error) {
	file := c.conf().Speech
	cfg, err := c.audioConfig(ctx, res, "RINGIL_SPEECH", file)
	if err != nil || cfg.Model == "" {
		return nil, err
	}
	voice, err := envOr(ctx, res, "RINGIL_SPEECH_VOICE", file.Voice)
	if err != nil {
		return nil, err
	}
	return openai.NewSynthesizer(cfg, voice)
}

// envOr is the variable name in the environment (or --env-file), else
// the file's value.
func envOr(ctx *orpheus.Context, res secrets.Resolver, name, file string) (string, error) {
	v, found, err := secrets.Lookup(ctx.Context(), settingsOf(res), name)
	if err != nil || found {
		return v, err
	}
	return file, nil
}

// audioConfig is an audio model's endpoint: <prefix>_MODEL (or the file's
// model) at <prefix>_PROVIDER (or the file's provider), else at the main
// model's provider, with that provider's key and no other. No model, no
// config.
func (c *cli) audioConfig(ctx *orpheus.Context, res secrets.Resolver, prefix string, file audioConfig) (openai.Config, error) {
	model, err := envOr(ctx, res, prefix+"_MODEL", file.Model)
	if err != nil || model == "" {
		return openai.Config{}, err
	}
	name, err := c.providerSetting(ctx, res, "", prefix+"_PROVIDER", file.Provider)
	if err == nil && name == "" {
		name, err = c.mainProviderName(ctx, res)
	}
	if err != nil {
		return openai.Config{}, err
	}
	ep, err := c.conf().endpointNamed(name)
	if err == nil && ep.kind != providerOpenAI {
		err = errors.New(ErrUsage, prefix+"_MODEL: "+name+" has no audio models; give an OpenAI-compatible provider ("+prefix+"_PROVIDER)")
	}
	if err != nil {
		return openai.Config{}, err
	}
	return openai.Config{BaseURL: ep.base, Model: model, KeyFunc: keyFunc(res, ep.keyName), AllowHTTP: c.flagBool(ctx, "allow-http")}, nil
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

// listens: a message trigger listens to chat (its key, channel.ChatKey).
func (c *cli) listens(chat string) bool {
	r := c.triggers.Load()
	return r != nil && r.Listens(chat)
}

// hear sets off the message triggers a message in chat matches.
func (c *cli) hear(chat, text string) {
	if r := c.triggers.Load(); r != nil {
		r.Heard(chat, text)
	}
}
