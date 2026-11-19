// bridge.go: chats as conversations of the server
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	stderrors "errors"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/serve"
)

// Error codes.
const (
	ErrConfig errors.ErrorCode = "CHANNEL_CONFIG"
	ErrQueue  errors.ErrorCode = "CHANNEL_QUEUE"
)

// Config sets up a Bridge.
type Config struct {
	Channel Channel
	// Server runs the agents; it must keep conversations (serve.Config.Store).
	Server *serve.Server
	// Profile is the server's profile the chats talk to; default "default".
	Profile string
	// Allow lists the senders (Message.From) who are heard. A message
	// from anyone else is not answered, and is reported as Refused, with
	// its sender, for the host to add if wanted: empty, nobody is heard,
	// which is how a user learns their own ID.
	Allow []string
	// OnEvent hears what the chats do not see. It is called from several
	// goroutines at once and must not block.
	OnEvent func(Event)
}

// EventKind is what happened.
type EventKind int

// Events.
const (
	// Refused: a message from a sender not allowed.
	Refused EventKind = iota + 1
	// ReceiveFailed: the channel could not receive; the Bridge retries.
	ReceiveFailed
	// SendFailed: a reply could not be sent.
	SendFailed
)

// Event is something the host should know.
type Event struct {
	Kind EventKind
	Chat string
	From string
	Err  error
}

// Bridge talks with one channel for a server.
type Bridge struct {
	cfg   Config
	allow map[string]bool

	mu    sync.Mutex
	chats map[string]*chat   // chats with a run going, by chat ID
	asks  map[string]*ask    // by token
	boxes map[string]*outbox // replies waiting, by chat ID
	wg    sync.WaitGroup
}

// chat is a chat with a run going.
type chat struct {
	run  string
	open []*ask // oldest first
}

// NewBridge checks cfg and returns a Bridge.
func NewBridge(cfg Config) (*Bridge, error) {
	switch {
	case cfg.Channel == nil || cfg.Server == nil:
		return nil, errors.New(ErrConfig, "a bridge needs a channel and a server")
	case !validName.MatchString(cfg.Channel.Name()):
		return nil, errors.New(ErrConfig, "the channel's name must be 1-20 lower-case letters, digits or -")
	}
	allow := map[string]bool{}
	for _, id := range cfg.Allow {
		if id = strings.TrimSpace(id); id != "" {
			allow[id] = true
		}
	}
	if cfg.Profile == "" {
		cfg.Profile = "default"
	}
	return &Bridge{cfg: cfg, allow: allow, chats: map[string]*chat{}, asks: map[string]*ask{}, boxes: map[string]*outbox{}}, nil
}

// Run receives and answers until ctx ends, then waits for the replies
// under way. The runs going are the server's: they end with it. Replies
// are sent apart from receiving: a slow chat never holds up the others.
func (b *Bridge) Run(ctx context.Context) error {
	defer b.wg.Wait()
	var wait time.Duration
	for {
		m, err := b.cfg.Channel.Receive(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			b.report(Event{Kind: ReceiveFailed, Err: err})
			wait = backoff(wait)
			pause(ctx, wait)
			continue
		}
		wait = 0
		b.handle(ctx, m)
	}
}

// backoff doubles the wait after a failure, from 1s to 1 minute.
func backoff(last time.Duration) time.Duration {
	return min(max(2*last, time.Second), time.Minute)
}

func pause(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func (b *Bridge) handle(ctx context.Context, m Message) {
	if !b.allow[m.From] {
		b.report(Event{Kind: Refused, Chat: m.Chat, From: m.From})
		return
	}
	if m.Pressed != "" {
		b.press(ctx, m)
		return
	}
	text := strings.TrimSpace(m.Text)
	switch {
	case text == "":
		b.say(ctx, m.Chat, "I can read only text messages.")
	case command(text, "/start"):
		b.say(ctx, m.Chat, "Hello. Write to me and I will answer; /stop stops what I am doing.")
	case command(text, "/stop"):
		b.stop(ctx, m.Chat)
	case b.answered(ctx, m.Chat, text):
	default:
		b.start(ctx, m.Chat, text)
	}
}

func (b *Bridge) stop(ctx context.Context, chatID string) {
	b.mu.Lock()
	c := b.chats[chatID]
	b.mu.Unlock()
	if c == nil {
		b.say(ctx, chatID, "Nothing to stop.")
		return
	}
	_ = b.cfg.Server.Cancel(c.run) // the run's end says it stopped
}

// start begins a run on the chat's conversation; one at a time per chat.
func (b *Bridge) start(ctx context.Context, chatID, text string) {
	b.mu.Lock()
	if b.chats[chatID] != nil {
		b.mu.Unlock()
		b.say(ctx, chatID, "I am still working on your last message; send /stop to stop it.")
		return
	}
	snap, err := b.cfg.Server.Start(b.cfg.Profile, Conversation(b.cfg.Channel.Name(), chatID), text)
	if err != nil {
		b.mu.Unlock()
		b.say(ctx, chatID, userMessage(err))
		return
	}
	b.chats[chatID] = &chat{run: snap.ID}
	b.wg.Add(1)
	b.mu.Unlock()
	go b.follow(ctx, chatID, snap.ID)
}

// follow asks the run's approvals in the chat and sends its answer.
func (b *Bridge) follow(ctx context.Context, chatID, run string) {
	defer b.wg.Done()
	asked := map[string]bool{}
	var last serve.Snapshot
	err := b.cfg.Server.Watch(ctx, run, func(s serve.Snapshot) {
		last = s
		b.prune(chatID, s.Pending)
		for _, p := range s.Pending {
			if !asked[p.ID] {
				asked[p.ID] = true
				b.ask(ctx, chatID, run, p)
			}
		}
	})
	b.mu.Lock()
	for _, a := range b.chats[chatID].open {
		delete(b.asks, a.token)
	}
	delete(b.chats, chatID)
	b.mu.Unlock()
	if err == nil {
		b.say(ctx, chatID, final(last))
	}
}

// final is what the chat reads when a run ends.
func final(s serve.Snapshot) string {
	switch {
	case s.State == serve.Cancelled:
		return "Stopped."
	case s.Error != nil:
		return s.Error.Message
	case strings.TrimSpace(s.Text) == "":
		return "Done."
	}
	return s.Text
}

// userMessage is the server's message for the user, never internal detail.
func userMessage(err error) string {
	var e *errors.Error
	if stderrors.As(err, &e) && e.UserMsg != "" {
		return e.UserMsg
	}
	return "The agent cannot answer now."
}

func (b *Bridge) report(e Event) {
	if b.cfg.OnEvent != nil {
		b.cfg.OnEvent(e)
	}
}
