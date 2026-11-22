// onboard_more.go: the system prompt, the memory, a Telegram bot
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agilira/argus"
	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/channel/telegram"
)

// askPrompt registers the file of the user's own instructions, added to
// the base prompt; none is fine.
func (s *setup) askPrompt() error {
	v, err := s.ask("Your system prompt's file (your instructions, added to ringil's; Enter for none)", "", func(v string) error {
		if v == "" {
			return nil
		}
		if err := argus.ValidateSecurePath(expandHome(s.c, v)); err != nil {
			return errors.New(ErrUsage, "not a path ringil reads: "+describe(err))
		}
		info, err := os.Stat(expandHome(s.c, v))
		switch {
		case err != nil || !info.Mode().IsRegular():
			return errors.New(ErrUsage, "no such file")
		case info.Size() > agent.MaxInstructions:
			return errors.New(ErrUsage, "at most 64 KiB")
		}
		return nil
	})
	if err != nil || v == "" {
		return err
	}
	s.prompt, err = filepath.Abs(expandHome(s.c, v))
	return err
}

// expandHome turns a leading ~ into the user's home.
func expandHome(c *cli, p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := c.home()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// chooseEmbedder offers, if wanted, an embedding model: the memory then
// finds by meaning, not by words alone. At the chat model's server (an
// OpenAI-compatible one), or at Ollama (with any provider: Anthropic has
// no embedding models).
func (s *setup) chooseEmbedder() error {
	options := []string{"no (the memory finds by words)", "Ollama on this machine"}
	if s.provider.kind == "openai" {
		options = append(options, "a model of the same server as the chat model")
	}
	i, err := s.choose("An embedding model too? The memory then finds by meaning (optional)", options)
	if err != nil || i == 0 {
		return err
	}
	if i == 2 {
		return s.sameServerEmbedder()
	}
	url, err := s.ask("Ollama's address", "http://127.0.0.1:11434", nonEmpty("an address"))
	if err != nil {
		return err
	}
	model, err := s.ask("The embedding model (pulled with `ollama pull`)", "qwen3-embedding:0.6b", nonEmpty("a model name"))
	s.embedder = embedderConfig{Kind: "ollama", URL: url, Model: model}
	return err
}

// sameServerEmbedder picks one of the server's models whose name says
// embedding, or one typed.
func (s *setup) sameServerEmbedder() error {
	ids, _ := s.models()
	var embeds []string
	for _, id := range ids {
		if strings.Contains(strings.ToLower(id), "embed") {
			embeds = append(embeds, id)
		}
	}
	if len(embeds) == 0 {
		var err error
		s.embedModel, err = s.ask("The embedding model's name", "", nonEmpty("a model name"))
		return err
	}
	shown := embeds[:min(len(embeds), 15)]
	i, err := s.choose("Which embedding model?", append(append([]string(nil), shown...), "another (type its name)"))
	if err != nil {
		return err
	}
	if i < len(shown) {
		s.embedModel = shown[i]
		return nil
	}
	s.embedModel, err = s.ask("The embedding model's name", "", nonEmpty("a model name"))
	return err
}

// chooseMemory: SQLite on this machine, Mem0 (its own model), or Chroma
// (by meaning: offered when an embedding model was chosen).
func (s *setup) chooseMemory() error {
	options := []string{"on this machine (SQLite: nothing to install)", "a Mem0 server"}
	if s.embedModel != "" || s.embedder.Kind != "" {
		options = append(options, "a Chroma server")
	}
	i, err := s.choose("Where should ringil keep what it remembers?", options)
	if err != nil || i == 0 {
		s.memory = memoryStore{Kind: "sqlite"}
		return err
	}
	kind := map[int]string{1: "mem0", 2: "chroma"}[i]
	url, err := s.ask("The "+kind+" server's address", map[string]string{"mem0": "http://127.0.0.1:8888", "chroma": "http://127.0.0.1:8000"}[kind], nonEmpty("an address"))
	if err != nil {
		return err
	}
	s.memory = memoryStore{Kind: kind, URL: url}
	return nil
}

// telegramWait is how long the setup waits for the owner's /start.
var telegramWait = 5 * time.Minute

// askTelegram sets up a Telegram bot, if wanted: its token goes to the
// store, and whoever writes /start to it first, in a private chat, is its
// owner (no one need look up their Telegram ID).
func (s *setup) askTelegram() error {
	yes, err := s.p.Confirm("Talk with ringil on Telegram too (a bot of yours, made with @BotFather)?", false)
	if err != nil || !yes {
		return err
	}
	token, err := s.p.AskSecret("The bot's token (not shown, kept in the store you chose)")
	if err != nil {
		return err
	}
	bot, err := s.c.newTelegram(s.ctx, telegram.Config{Token: token})
	if err != nil {
		return errors.Wrap(err, ErrUsage, "Telegram refused the token: "+describe(err))
	}
	if err := s.keys.Set(s.ctx, "RINGIL_TELEGRAM_TOKEN", token); err != nil {
		return err
	}
	if err := s.c.line("  now write /start to @" + bot.Username() + " from your phone (waiting up to 5 minutes)"); err != nil {
		return err
	}
	owner, err := waitStart(s.ctx, bot)
	if err != nil {
		return err
	}
	s.owners = []string{owner}
	return s.c.line("  you are the bot's owner")
}

// telegramBot is what the setup needs of a bot; tests replace it.
type telegramBot interface {
	channel.Channel
	Username() string
}

// waitStart is the sender of the first /start in a private chat, told
// they are the owner.
func waitStart(parent context.Context, bot telegramBot) (string, error) {
	ctx, cancel := context.WithTimeout(parent, telegramWait)
	defer cancel()
	for {
		m, err := bot.Receive(ctx)
		if err != nil {
			if parent.Err() != nil {
				return "", parent.Err() // interrupted
			}
			if ctx.Err() != nil {
				return "", errors.New(ErrUsage, "no /start came: run `ringil setup` again when ready")
			}
			return "", err
		}
		if m.Group || m.From == "" || !strings.HasPrefix(strings.TrimSpace(m.Text), "/start") {
			continue
		}
		if err := bot.Send(ctx, m.Chat, channel.Out{Text: "ringil: you are this bot's owner. Your messages here go to your agent."}); err != nil {
			return "", err
		}
		return m.From, nil
	}
}
