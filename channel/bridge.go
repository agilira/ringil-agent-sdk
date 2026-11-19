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

	agent "github.com/agilira/ringil-agent-sdk"
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
	// Transcriber turns voice messages into text; nil, the chat is told
	// they cannot be heard.
	Transcriber agent.Transcriber
	// Inbox is the directory where the documents sent in the chats are
	// saved, each conversation in its own directory (Bridge.InboxOf), for the
	// agent to read; the agent is told where. Empty, the chat is told
	// documents are not taken.
	Inbox string
	// Synthesizer speaks answers. With it, a voice message is answered
	// with voice when VoiceReplies is set or the chat asked for it with
	// /voice (and not with /text); every other answer is text.
	Synthesizer  agent.Synthesizer
	VoiceReplies bool
	// Sessions is the file where each chat's conversation number and last
	// message are kept, so that /new and IdleReset hold across restarts;
	// empty keeps them in memory only.
	Sessions string
	// IdleReset starts a new conversation at the first message after a
	// silence this long (the chat is told): default DefaultIdleReset (24
	// hours), negative never. /new starts one at any time.
	IdleReset time.Duration
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
	// FileFailed: a file sent in a chat could not be fetched, heard or
	// saved; the chat is told.
	FileFailed
	// VoiceFailed: an answer could not be spoken or sent as voice; it
	// went as text.
	VoiceFailed
	// SessionsFailed: the sessions file could not be written; the chats
	// go on, but a restart forgets their /new and silences.
	SessionsFailed
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
	voice map[string]bool    // /voice and /text, by chat ID
	// sessions are the chats' conversations, by chat ID.
	sessions   map[string]session
	reconciled map[string]bool // chats whose number was checked against the server
	saving     sync.Mutex      // one write of the sessions file at a time
	now        func() time.Time
	wg         sync.WaitGroup
}

// chat is a chat with a run going, or its message being prepared (its
// files fetched): run is empty until it starts.
type chat struct {
	conv   string // the conversation the message goes to
	run    string
	cancel context.CancelFunc // stops the preparing
	open   []*ask             // oldest first
}

// NewBridge checks cfg and returns a Bridge.
func NewBridge(cfg Config) (*Bridge, error) {
	switch {
	case cfg.Channel == nil || cfg.Server == nil:
		return nil, errors.New(ErrConfig, "a bridge needs a channel and a server")
	case !validName.MatchString(cfg.Channel.Name()):
		return nil, errors.New(ErrConfig, "the channel's name must be 1-20 lower-case letters or digits")
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
	sessions, err := loadSessions(cfg.Sessions)
	if err != nil {
		return nil, err
	}
	return &Bridge{cfg: cfg, allow: allow, chats: map[string]*chat{}, asks: map[string]*ask{}, boxes: map[string]*outbox{},
		voice: map[string]bool{}, sessions: sessions, reconciled: map[string]bool{}, now: time.Now}, nil
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
	case text == "" && len(m.Files) == 0:
		b.say(ctx, m.Chat, "I cannot read this kind of message.")
	case len(m.Files) == 0 && b.command(ctx, m.Chat, text):
	case len(m.Files) == 0 && b.answered(ctx, m.Chat, text):
	default:
		b.start(ctx, m.Chat, text, m.Files)
	}
}

// command runs a chat command; false when text is not one.
func (b *Bridge) command(ctx context.Context, chatID, text string) bool {
	switch {
	case command(text, "/start"):
		b.say(ctx, chatID, "Hello. Write to me and I will answer. /new starts a new conversation, /stop stops what I am doing, /voice and /text choose how I answer voice messages.")
	case command(text, "/stop"):
		b.stop(ctx, chatID)
	case command(text, "/new"):
		b.renew(ctx, chatID)
	case command(text, "/voice"):
		b.setVoice(ctx, chatID, true)
	case command(text, "/text"):
		b.setVoice(ctx, chatID, false)
	default:
		return false
	}
	return true
}

func (b *Bridge) stop(ctx context.Context, chatID string) {
	b.mu.Lock()
	c := b.chats[chatID]
	var run string
	if c != nil {
		run = c.run
		if run == "" {
			c.cancel()
		}
	}
	b.mu.Unlock()
	switch {
	case c == nil:
		b.say(ctx, chatID, "Nothing to stop.")
	case run != "":
		_ = b.cfg.Server.Cancel(run) // the run's end says it stopped
	}
}

// start prepares the message and runs it on the chat's conversation;
// one at a time per chat.
func (b *Bridge) start(ctx context.Context, chatID, text string, files []File) {
	b.mu.Lock()
	if b.chats[chatID] != nil {
		b.mu.Unlock()
		b.say(ctx, chatID, "I am still working on your last message; send /stop to stop it.")
		return
	}
	b.mu.Unlock()
	conv, renewed := b.touch(ctx, chatID)
	if renewed {
		b.say(ctx, chatID, "This chat was quiet for a while, so this is a new conversation: I no longer see the earlier messages.")
	}
	b.mu.Lock() // still free: only Run's goroutine starts runs
	prep, cancel := context.WithCancel(ctx)
	b.chats[chatID] = &chat{conv: conv, cancel: cancel}
	b.wg.Add(1)
	b.mu.Unlock()
	go b.converse(ctx, prep, chatID, text, files)
}

// converse fetches the message's files, starts its run and follows it.
func (b *Bridge) converse(ctx, prep context.Context, chatID, text string, files []File) {
	defer b.wg.Done()
	typing := b.typing(ctx, chatID)
	defer typing.stop()
	run, reply := b.launch(ctx, prep, chatID, text, files)
	spoken := hasVoice(files)
	if run == "" {
		typing.stop() // before the reply, which ends what the app shows
		if reply != "" {
			b.say(ctx, chatID, reply)
		}
		return
	}
	b.follow(ctx, chatID, run, typing, spoken)
}

// launch builds the message and starts its run; without a run, the chat
// is free again and reply says why.
func (b *Bridge) launch(ctx, prep context.Context, chatID, text string, files []File) (run, reply string) {
	msg, problem := b.message(prep, chatID, text, files)
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.chats[chatID]
	stopped := prep.Err() != nil && ctx.Err() == nil // by /stop, not by the end of Run
	c.cancel()
	switch {
	case ctx.Err() != nil: // Run is ending: no run nobody would follow
	case stopped:
		reply = "Stopped."
	case problem != "":
		reply = problem
	default:
		snap, err := b.cfg.Server.StartMessage(b.cfg.Profile, c.conv, msg)
		if err == nil {
			c.run = snap.ID
			return snap.ID, ""
		}
		reply = userMessage(err)
	}
	delete(b.chats, chatID)
	return "", reply
}

// follow asks the run's approvals in the chat and sends its answer.
// A voice message (spoken) may be answered with voice.
func (b *Bridge) follow(ctx context.Context, chatID, run string, typing *typer, spoken bool) {
	asked := map[string]bool{}
	var last serve.Snapshot
	err := b.cfg.Server.Watch(ctx, run, func(s serve.Snapshot) {
		last = s
		typing.waiting.Store(len(s.Pending) > 0)
		b.prune(chatID, s.Pending)
		for _, p := range s.Pending {
			if !asked[p.ID] {
				asked[p.ID] = true
				b.ask(ctx, chatID, run, p)
			}
		}
	})
	// The chat stays busy until its answer is posted, in order; /stop
	// now cancels the speaking (the answer then goes as text).
	speaking, cancel := context.WithCancel(ctx)
	defer cancel()
	b.mu.Lock()
	c := b.chats[chatID]
	for _, a := range c.open {
		delete(b.asks, a.token)
	}
	c.open, c.run, c.cancel = nil, "", cancel
	b.mu.Unlock()
	if err == nil {
		out := Out{Text: final(last)}
		if spoken && last.State == serve.Completed {
			out.Voice = b.speak(speaking, chatID, out.Text)
		}
		typing.stop() // before the reply, which ends what the app shows
		b.post(ctx, chatID, out, nil)
	}
	typing.stop()
	b.mu.Lock()
	delete(b.chats, chatID)
	b.mu.Unlock()
	b.stamp(chatID)
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
