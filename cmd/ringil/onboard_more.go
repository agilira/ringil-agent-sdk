// onboard_more.go: the system prompt, the memory, a Telegram bot
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

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
		return checkPromptFile(expandHome(s.c, v))
	})
	if err != nil || v == "" {
		return err
	}
	s.prompt, err = filepath.Abs(expandHome(s.c, v))
	return err
}

func checkPromptFile(path string) error {
	if err := checkPromptPath(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !private(info) {
		return errors.New(ErrUsage, "instructions must be a regular private file")
	}
	if info.Size() > agent.MaxInstructions {
		return errors.New(ErrUsage, "at most 64 KiB")
	}
	data, err := os.ReadFile(path) // #nosec G304 -- user-provided instructions file checked by ValidateSecurePath and Lstat
	if err != nil {
		return errors.New(ErrUsage, "cannot read instructions: "+describe(err))
	}
	if len(data) > agent.MaxInstructions || !utf8.Valid(data) {
		return errors.New(ErrUsage, "instructions must be UTF-8 text of at most 64 KiB")
	}
	return nil
}

func checkPromptPath(path string) error {
	if strings.ContainsAny(path, "*?[") {
		return errors.New(ErrUsage, "an instructions file's path cannot hold * ? or [: "+path)
	}
	if err := argus.ValidateSecurePath(path); err != nil {
		return errors.New(ErrUsage, "not a path ringil reads: "+describe(err))
	}
	return nil
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
	if s.endpoint.kind == providerOpenAI {
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

// telegramWait is how long the setup waits for the owner's /start: the
// pairing code's life.
var telegramWait = 5 * time.Minute

// askTelegram sets up a Telegram bot, if wanted: its token goes to the
// store, and whoever sends /start with the code the setup shows, in a
// private chat, is its owner (no one need look up their Telegram ID).
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
	if err := s.keep("RINGIL_TELEGRAM_TOKEN", token); err != nil {
		return err
	}
	owner, err := s.pair(bot)
	if err != nil {
		return err
	}
	s.owners = []string{owner}
	return s.c.line("  you are the bot's owner")
}

// pair waits for the owner: a /start with a code of this setup's, sent
// after it began (what was waiting is dropped: anyone can write to a
// bot, and might have first).
func (s *setup) pair(bot telegramBot) (string, error) {
	if err := bot.Discard(s.ctx); err != nil {
		return "", errors.Wrap(err, ErrUsage, "Telegram: "+describe(err))
	}
	code, err := pairingCode()
	if err != nil {
		return "", err
	}
	if err := s.c.line("  now open https://t.me/" + bot.Username() + "?start=" + code + " on your phone, or write to @" + bot.Username() +
		": /start " + code + " (waiting up to 5 minutes)"); err != nil {
		return "", err
	}
	return waitStart(s.ctx, bot, code)
}

// telegramBot is what the setup needs of a bot; tests replace it.
type telegramBot interface {
	channel.Channel
	Username() string
	Discard(ctx context.Context) error
}

// pairingAlphabet has no look-alikes (0 O, 1 I L): read off a screen,
// typed on a phone.
const pairingAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// pairingCode is 8 random letters and digits (31^8, some 2^39), for one
// pairing.
func pairingCode() (string, error) {
	b := make([]byte, 8)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(pairingAlphabet))))
		if err != nil {
			return "", errors.Wrap(err, ErrUsage, "no pairing code")
		}
		b[i] = pairingAlphabet[n.Int64()]
	}
	return string(b), nil
}

// waitStart is the sender of the first /start <code> in a private chat,
// told they are the owner; any other message is ignored.
func waitStart(parent context.Context, bot telegramBot, code string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, telegramWait)
	defer cancel()
	for {
		m, err := bot.Receive(ctx)
		if err != nil {
			if parent.Err() != nil {
				return "", parent.Err() // interrupted
			}
			if ctx.Err() != nil {
				return "", errors.New(ErrUsage, "no /start with the code came: run `ringil setup` again when ready")
			}
			return "", err
		}
		if m.Group || m.From == "" || !pairs(m.Text, code) {
			continue
		}
		if err := bot.Send(ctx, m.Chat, channel.Out{Text: "ringil: you are this bot's owner. Your messages here go to your agent."}); err != nil {
			return "", err
		}
		return m.From, nil
	}
}

// pairs: text is /start and the code (any case), compared in
// constant time.
func pairs(text, code string) bool {
	f := strings.Fields(text)
	if len(f) != 2 || (f[0] != "/start" && !strings.HasPrefix(f[0], "/start@")) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.ToUpper(f[1])), []byte(code)) == 1
}
