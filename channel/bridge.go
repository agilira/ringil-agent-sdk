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
	"sync/atomic"
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
	// Owners are the senders (Message.From) with every right: they talk
	// with Profile (default "default") in their own chats, and only they
	// approve tool calls.
	Owners  []string
	Profile string
	// Members are senders who talk with MemberProfile, which the host
	// gives the tools members may use; they never approve. IsMember, when
	// set, is asked about senders not listed (such as the members of a
	// company group), and its answers are kept for MemberCheck (default
	// 10 minutes). Members need a MemberProfile.
	Members       []string
	IsMember      func(ctx context.Context, from string) (bool, error)
	MemberCheck   time.Duration
	MemberProfile string
	// MemberPhones are phone numbers of members, with their country code
	// ("+39 333 1234567", never with a trunk "(0)"). Anyone else writing
	// in their own chat is asked to share their number, with the app's
	// button (the Channel must be a PhoneAsker); on a public bot, at
	// /start. Who shares a listed one is a member from then on, by their
	// sender ID, kept in the file SharedPhones (empty: in memory only);
	// taking the number off the list takes them out, also when they
	// changed number since.
	MemberPhones []string
	SharedPhones string
	// PublicProfile, when set, answers anyone else: a public bot. Without
	// it, a message from anyone else is not answered, and is reported as
	// Refused with its sender, for the host to add if wanted (which is how
	// a user learns their own ID).
	PublicProfile string
	// MaxPerHour bounds the messages a member or public sender has
	// answered in an hour; default 30, negative no bound. Owners have none.
	MaxPerHour int
	// Owners, Members, MemberProfile, MemberPhones, PublicProfile,
	// MaxPerHour and NoApprovals can change while the Bridge runs:
	// Bridge.SetPeople.
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
	// Automations, when set, lists the host's automations, shown by
	// /automate alone; "" when there are none.
	Automations func() string
	// Listens, when set, says whether something listens to a chat's
	// messages (the host's message triggers), by its key, ChatKey; Hear
	// then hears each message there of an owner or a member (strangers
	// and the public never), commands aside. A group's messages not
	// addressed to the bot are heard only if the app delivers them (on
	// Telegram: privacy mode off, or the bot an admin).
	Listens func(chat string) bool
	Hear    func(chat, text string)
	// NoApprovals allows, without asking, the tool calls of the runs an
	// owner starts; the others are still refused. In the chat, "Yes,
	// always" allows the same again for the rest of the conversation: the
	// same file, the same command, the same domain; for another tool,
	// that tool.
	NoApprovals bool
	// The Bridge says its own texts in each user's language, as the app
	// says it (Message.Language): English, Italian, or one of Languages,
	// the host's translations of Texts() by language code ("de"); a user
	// in a language it does not speak is spoken to in English. Language
	// is the one used when the app does not say (default English).
	Language  string
	Languages map[string]map[string]string
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
	// Limited: a member or public sender went past MaxPerHour.
	Limited
	// MemberCheckFailed: IsMember failed; the sender was not let in.
	MemberCheckFailed
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
	// PhonesFailed: the SharedPhones file could not be written; a restart
	// forgets who shared their phone since.
	PhonesFailed
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
	cfg Config
	who atomic.Pointer[people] // who it talks with (SetPeople)

	mu      sync.Mutex
	chats   map[string]*chat       // chats with a run going, by chat ID
	asks    map[string]*ask        // by token
	boxes   map[string]*outbox     // replies waiting, by chat ID
	voice   map[string]bool        // /voice and /text, by chat ID
	checked map[string]memberCheck // IsMember's answers, by sender
	recent  map[string][]time.Time // messages answered in the last hour, by sender
	shared  map[string]string      // listed phones shared, by sender
	always  map[string]bool        // calls allowed for the rest of a conversation, by conversation and key
	// sessions are the chats' conversations, by chat ID.
	sessions   map[string]session
	backs      map[string]back   // where checks come back, by conversation (Private)
	backOrder  []string          // backs' keys, oldest first
	reconciled map[string]bool   // chats whose number was checked against the server
	saving     sync.Mutex        // one write of the sessions file at a time
	spoken     map[string]speech // the languages spoken, by code
	running    context.Context   // Run's, while it runs (Notify); under mu
	langMu     sync.Mutex
	langs      map[string]string // the language of each chat's last sender, by chat ID; under langMu
	now        func() time.Time
	wg         sync.WaitGroup
}

// chat is a chat with a run going, or its message being prepared (its
// files fetched): run is empty until it starts.
type chat struct {
	conv   string // the conversation the message goes to
	source string // what the run is, for the host: Automate, or ""
	group  bool
	role   role   // of who started the run
	from   string // who started it
	run    string
	cancel context.CancelFunc // stops the preparing
	open   []*ask             // oldest first
}

// NewBridge checks cfg and returns a Bridge.
func NewBridge(cfg Config) (*Bridge, error) {
	if err := checkConfig(cfg); err != nil {
		return nil, err
	}
	who, err := peopleOf(cfg, PeopleOf(cfg))
	if err != nil {
		return nil, err
	}
	if cfg.Profile == "" {
		cfg.Profile = "default"
	}
	sessions, err := loadSessions(cfg.Sessions)
	if err != nil {
		return nil, err
	}
	shared, pruned, err := loadShared(cfg.SharedPhones, who.phones)
	if err != nil {
		return nil, err
	}
	speaks, err := spoken(cfg)
	if err != nil {
		return nil, err
	}
	b := &Bridge{cfg: cfg, shared: shared, always: map[string]bool{}, langs: map[string]string{}, spoken: speaks, chats: map[string]*chat{}, asks: map[string]*ask{},
		boxes: map[string]*outbox{}, voice: map[string]bool{}, checked: map[string]memberCheck{}, recent: map[string][]time.Time{},
		sessions: sessions, reconciled: map[string]bool{}, backs: map[string]back{}, now: time.Now}
	b.who.Store(who)
	if pruned {
		b.persistShared() // the numbers taken off the list leave the disk too
	}
	return b, nil
}

// spoken is the languages the Bridge speaks, Config.Language among them.
func spoken(cfg Config) (map[string]speech, error) {
	speaks, err := languages(cfg.Languages)
	if err != nil {
		return nil, err
	}
	if _, ok := speaks[cfg.Language]; !ok && cfg.Language != "" {
		return nil, errors.New(ErrConfig, "the Bridge does not speak "+cfg.Language+": add it to Languages")
	}
	return speaks, nil
}

func checkConfig(cfg Config) error {
	switch {
	case cfg.Channel == nil || cfg.Server == nil:
		return errors.New(ErrConfig, "a bridge needs a channel and a server")
	case !validName.MatchString(cfg.Channel.Name()):
		return errors.New(ErrConfig, "the channel's name must be 1-20 lower-case letters or digits")
	}
	return nil
}

// Run receives and answers until ctx ends, then waits for the replies
// under way. The runs going are the server's: they end with it. Replies
// are sent apart from receiving: a slow chat never holds up the others.
func (b *Bridge) Run(ctx context.Context) error {
	defer b.wg.Wait()
	b.mu.Lock()
	b.running = ctx
	b.mu.Unlock()
	defer func() { // before the wait: no Notify adds to it after
		b.mu.Lock()
		b.running = nil
		b.mu.Unlock()
	}()
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
	b.overhear(m)
	if m.Group && !m.Addressed && m.Pressed == "" {
		return // group talk not for the bot
	}
	b.hearLanguage(m.Chat, m.Language)
	r := b.roleOf(ctx, m.From)
	switch {
	case b.sharesPhone(m) && r != owner && r != member:
		b.sharedPhone(ctx, m)
	case r == stranger:
		b.report(Event{Kind: Refused, Chat: m.Chat, From: m.From})
		b.askPhone(ctx, m, mAskPhone)
	case m.Pressed != "":
		b.press(ctx, m, r)
	default:
		b.dispatch(ctx, m, r)
	}
}

// dispatch handles a message of a sender let in.
func (b *Bridge) dispatch(ctx context.Context, m Message, r role) {
	text := strings.TrimSpace(m.Text)
	switch {
	case len(m.Files) == 0 && r == owner && b.answered(ctx, m.Chat, text):
	case !b.within(m.From, r): // commands count too
		b.report(Event{Kind: Limited, Chat: m.Chat, From: m.From})
		b.say(ctx, m.Chat, b.tr(m.Chat, mTooMany))
	case text == "" && len(m.Files) == 0:
		b.say(ctx, m.Chat, b.tr(m.Chat, mUnreadable))
	case b.commanded(ctx, m, r, text):
	default:
		b.start(ctx, m, r)
	}
}

// commanded runs /automate (which may bring files) or another command
// (which may not); false when text is no command.
func (b *Bridge) commanded(ctx context.Context, m Message, r role, text string) bool {
	return b.automate(ctx, m, r, text) || (len(m.Files) == 0 && b.command(ctx, m, r, text))
}

// command runs a chat command; false when text is not one. In a group,
// which several people share, the public cannot start a new
// conversation, and only an owner or who sent the message stops a run.
func (b *Bridge) command(ctx context.Context, m Message, r role, text string) bool {
	chatID := m.Chat
	switch {
	case b.notHere(m, r, text):
		b.say(ctx, chatID, b.tr(chatID, mOwnerOnly))
	case command(text, "/start"):
		b.say(ctx, chatID, b.tr(chatID, mHello))
		if r == public {
			b.askPhone(ctx, m, mHelloPhone)
		}
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
		b.say(ctx, chatID, b.tr(chatID, mNothingStop))
	case run != "":
		_ = b.cfg.Server.Cancel(run) // the run's end says it stopped
	}
}

// notHere: a command the sender may not give in a group.
func (b *Bridge) notHere(m Message, r role, text string) bool {
	switch {
	case !m.Group || r == owner:
		return false
	case command(text, "/new"):
		return r == public
	case command(text, "/stop"):
		return !b.startedBy(m.Chat, m.From)
	}
	return false
}

// startedBy says whether from started the chat's run.
func (b *Bridge) startedBy(chatID, from string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.chats[chatID]
	return c != nil && c.from == from
}

// start prepares the message and runs it on the chat's conversation;
// one at a time per chat.
func (b *Bridge) start(ctx context.Context, m Message, r role) {
	b.startFrom(ctx, m, r, "")
}

// startFrom is start for a run labelled with its source (Automate).
func (b *Bridge) startFrom(ctx context.Context, m Message, r role, source string) {
	chatID, text, files := m.Chat, strings.TrimSpace(m.Text), m.Files
	if m.Group && b.groupProfile() == "" {
		b.say(ctx, chatID, b.tr(chatID, mNoGroups))
		return
	}
	b.mu.Lock()
	if b.chats[chatID] != nil {
		b.mu.Unlock()
		b.say(ctx, chatID, b.tr(chatID, mBusy))
		return
	}
	b.mu.Unlock()
	conv, renewed := b.touch(ctx, chatID)
	if renewed {
		b.say(ctx, chatID, b.tr(chatID, mQuiet))
	}
	b.mu.Lock()
	if b.chats[chatID] != nil { // a check came back meanwhile (ComeBack)
		b.mu.Unlock()
		b.say(ctx, chatID, b.tr(chatID, mBusy))
		return
	}
	prep, cancel := context.WithCancel(ctx)
	b.chats[chatID] = &chat{conv: conv, group: m.Group, role: r, from: m.From, cancel: cancel, source: source}
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
		reply = b.tr(chatID, mStopped)
	case problem != "":
		reply = problem
	default:
		snap, err := b.cfg.Server.StartFrom(c.source, b.profileFor(c.role, c.group), c.conv, msg)
		if err == nil {
			c.run = snap.ID
			return snap.ID, ""
		}
		reply = b.userMessage(chatID, err)
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
	// The chat stays busy until its answer is queued: what the chat sends
	// next is answered after it. /stop now cancels the speaking (the
	// answer then goes as text).
	speaking, cancel := context.WithCancel(ctx)
	defer cancel()
	b.mu.Lock()
	c := b.chats[chatID]
	for _, a := range c.open {
		delete(b.asks, a.token)
	}
	c.open, c.run, c.cancel = nil, "", cancel
	b.mu.Unlock()
	var out Out
	if err == nil {
		out.Text, out.Markdown = b.final(chatID, last), last.State == serve.Completed
		if spoken && last.State == serve.Completed {
			out.Voice = b.speak(speaking, chatID, out.Text)
		}
	}
	typing.stop() // before the reply, which ends what the app shows
	b.mu.Lock()
	delete(b.chats, chatID)
	if err == nil && !quiet(c.source, out.Text) {
		b.queue(ctx, chatID, outgoing{out: out}) // under b.mu: queued before any later reply of the chat
	}
	b.mu.Unlock()
	b.stamp(chatID)
}

// final is what the chat reads when a run ends.
func (b *Bridge) final(chatID string, s serve.Snapshot) string {
	switch {
	case s.State == serve.Cancelled:
		return b.tr(chatID, mStopped)
	case s.Error != nil:
		return s.Error.Message
	case strings.TrimSpace(s.Text) == "":
		return b.tr(chatID, mDone)
	}
	return s.Text
}

// userMessage is the server's message for the user, never internal detail.
func (b *Bridge) userMessage(chatID string, err error) string {
	var e *errors.Error
	if stderrors.As(err, &e) && e.UserMsg != "" {
		return e.UserMsg
	}
	return b.tr(chatID, mCannotAnswer)
}

func (b *Bridge) report(e Event) {
	if b.cfg.OnEvent != nil {
		b.cfg.OnEvent(e)
	}
}

// ErrStopped: the Bridge is not running (Notify).
const ErrStopped errors.ErrorCode = "CHANNEL_STOPPED"

// Notify sends out to a chat on the host's behalf (what an automation
// found), in order with the chat's replies, while Run runs.
func (b *Bridge) Notify(chatID string, out Out) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running == nil {
		return errors.New(ErrStopped, "the bridge is not running")
	}
	b.queue(b.running, chatID, outgoing{out: out})
	return nil
}

// Automated sends a chat what a run of the automation name answered, or,
// when failure is not empty, that it failed and why, under a line in the
// chat's language naming it.
func (b *Bridge) Automated(chatID, name, answer, failure string) error {
	if failure != "" {
		return b.Notify(chatID, Out{Text: b.tr(chatID, mAutomationFailed, name, failure)})
	}
	return b.Notify(chatID, Out{Text: b.tr(chatID, mAutomationSays, name) + "\n\n" + answer, Markdown: true})
}
