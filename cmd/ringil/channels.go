// channels.go: chat apps talking with the served agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/channel/telegram"
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
	allow := splitList(ctx.GetFlagString("telegram-allow"))
	b, err := channel.NewBridge(channel.Config{Channel: tg, Server: srv, Profile: a2aProfile(ctx), Allow: allow, OnEvent: c.channelEvent})
	if err != nil {
		return nil, err
	}
	run, cancel := context.WithCancel(run)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.Run(run)
	}()
	if len(allow) == 0 {
		c.note("telegram connected; nobody is allowed yet: write to the bot, then restart with --telegram-allow <your ID>")
	} else {
		c.note("telegram connected")
	}
	return func() { cancel(); <-done }, nil
}

func (c *cli) channelEvent(e channel.Event) {
	switch e.Kind {
	case channel.Refused:
		c.note("telegram: message from user " + clean(e.From) + " not allowed (to allow it: --telegram-allow " + clean(e.From) + ")")
	case channel.ReceiveFailed:
		c.note("telegram: cannot receive, retrying: " + clean(oneLine(e.Err.Error(), 300)))
	case channel.SendFailed:
		c.note("telegram: cannot send to chat " + clean(e.Chat) + ": " + clean(oneLine(e.Err.Error(), 300)))
	}
}
