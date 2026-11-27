// slack.go: the Slack app as a chat channel of serve
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/channel/slack"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/serve"
)

// startSlack connects the Slack app when both its tokens are set. Its
// bridge delivers the automations when Telegram has none.
func (c *cli) startSlack(ctx *orpheus.Context, run context.Context, res secrets.Resolver, srv *serve.Server) (func(), error) {
	bot, app, found, err := slackTokens(run, storeOnly(res)) // planted tokens would start someone else's app
	if err != nil || !found {
		return func() {}, err
	}
	sl, err := slack.New(run, slack.Config{BotToken: bot, AppToken: app})
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "slack: "+err.Error())
	}
	cfg, err := c.bridgeConfig(ctx, res, "slack")
	if err != nil {
		return nil, err
	}
	cfg.Channel, cfg.Server = sl, srv
	b, err := channel.NewBridge(cfg)
	if err != nil {
		return nil, err
	}
	c.slackBridge.Store(b)
	c.bridge.CompareAndSwap(nil, b)
	stop := background(run, b.Run)
	if len(cfg.Owners) == 0 {
		c.note("slack connected; no owner yet: write to the app, then add your member ID to serve.slack.owners in ringil.yaml")
	} else {
		c.note("slack connected as " + sl.BotUser())
	}
	return func() { stop(); _ = sl.Close() }, nil
}

// slackTokens are the bot and app tokens; found only when both are set.
func slackTokens(ctx context.Context, res secrets.Resolver) (bot, app string, found bool, err error) {
	bot, hasBot, err := secrets.Lookup(ctx, res, "SLACK_BOT_TOKEN")
	if err != nil {
		return "", "", false, err
	}
	app, hasApp, err := secrets.Lookup(ctx, res, "SLACK_APP_TOKEN")
	if err != nil {
		return "", "", false, err
	}
	return bot, app, hasBot && hasApp, nil
}
