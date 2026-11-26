// sessions.go: a chat's conversations, one after another
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
)

// DefaultIdleReset is how long a chat stays silent before its next
// message starts a new conversation.
const DefaultIdleReset = 24 * time.Hour

// maxSessionsFile bounds the sessions file read back.
const maxSessionsFile = 16 << 20

// session is where a chat is: its conversation's number and its last
// exchange.
type session struct {
	N    int       `json:"n"`
	Last time.Time `json:"last"`
}

// maxSession bounds a conversation's number: past it, the file is not
// one the Bridge wrote.
const maxSession = 1_000_000

// loadSessions reads the sessions file; none yet is no sessions.
func loadSessions(path string) (map[string]session, error) {
	out := map[string]session{}
	data, err := readState(path, maxSessionsFile, "sessions")
	switch {
	case err != nil:
		return nil, err
	case data == nil:
		return out, nil
	case json.Unmarshal(data, &out) != nil || out == nil || !validSessions(out):
		return nil, errors.New(ErrConfig, "the sessions file is damaged: "+path)
	}
	return out, nil
}

// readState reads a file the Bridge keeps (what names it); nil when there
// is none yet, or no path.
func readState(path string, max int64, what string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if stderrors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, ErrConfig, "cannot open the "+what+" file's directory")
	}
	defer func() { _ = root.Close() }()
	data, err := atomicfile.Read(root, filepath.Base(path), max)
	switch {
	case stderrors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, errors.Wrap(err, ErrConfig, "cannot read the "+what+" file")
	}
	return data, nil
}

func validSessions(m map[string]session) bool {
	for _, s := range m {
		if s.N < 1 || s.N > maxSession {
			return false
		}
	}
	return true
}

// persist writes the sessions, outside b.mu: one write at a time, each
// of the state when it starts, so the last written is the latest.
func (b *Bridge) persist() {
	if b.cfg.Sessions == "" {
		return
	}
	b.saving.Lock()
	defer b.saving.Unlock()
	b.mu.Lock()
	data, err := json.Marshal(b.sessions)
	b.mu.Unlock()
	if err == nil {
		err = writeFile(b.cfg.Sessions, data)
	}
	if err != nil {
		b.report(Event{Kind: SessionsFailed, Err: err})
	}
}

func writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return atomicfile.Write(root, filepath.Base(path), data)
}

// touch records a message of the chat and returns its conversation: a
// new one when the chat was silent past IdleReset since its last
// exchange (renewed).
func (b *Bridge) touch(ctx context.Context, chatID string) (conversation string, renewed bool) {
	b.reconcile(ctx, chatID)
	b.mu.Lock()
	s := b.sessions[chatID]
	now := b.now()
	if idle := b.idleReset(); idle > 0 && !s.Last.IsZero() && now.Sub(s.Last) > idle {
		s.N++
		renewed = true
	}
	s.N, s.Last = max(s.N, 1), now
	if len(b.sessions) >= maxTracked {
		b.forgetStale(now)
	}
	b.sessions[chatID] = s
	b.mu.Unlock()
	b.persist()
	return Conversation(b.cfg.Channel.Name(), chatID, s.N), renewed
}

// forgetStale drops the chats silent past IdleReset (a public bot hears
// from anyone): their next message starts a new conversation anyway, and
// reconcile finds its number on the server. b.mu held.
func (b *Bridge) forgetStale(now time.Time) {
	idle := b.idleReset()
	if idle < 0 {
		idle = 30 * 24 * time.Hour
	}
	for id, s := range b.sessions {
		if now.Sub(s.Last) > idle {
			delete(b.sessions, id)
			delete(b.reconciled, id)
		}
	}
}

// stamp records an exchange of the chat (a press, an answer, a reply):
// the silence counts from the last one.
func (b *Bridge) stamp(chatID string) {
	b.mu.Lock()
	s, ok := b.sessions[chatID]
	if ok {
		s.Last = b.now()
		b.sessions[chatID] = s
	}
	b.mu.Unlock()
	if ok {
		b.persist()
	}
}

// reconcile, once per chat, lifts its number to the highest conversation
// the server keeps: a lost or old sessions file never sends a new
// conversation into an old one.
func (b *Bridge) reconcile(ctx context.Context, chatID string) {
	b.mu.Lock()
	done := b.reconciled[chatID]
	if len(b.reconciled) >= maxTracked {
		clear(b.reconciled) // only spares listings: asked again
	}
	b.reconciled[chatID] = true
	b.mu.Unlock()
	if done {
		return
	}
	list, err := b.cfg.Server.Conversations(ctx)
	if err != nil {
		b.report(Event{Kind: SessionsFailed, Chat: chatID, Err: err})
		return
	}
	ids := make([]string, len(list))
	for i, c := range list {
		ids[i] = c.ID
	}
	top := highest(ids, Conversation(b.cfg.Channel.Name(), chatID, 1))
	b.mu.Lock()
	if s := b.sessions[chatID]; top > s.N {
		s.N = top
		b.sessions[chatID] = s
	}
	b.mu.Unlock()
}

// highest is the largest number of the conversations of first (its
// number 1) among ids; 0 when none.
func highest(ids []string, first string) int {
	top := 0
	for _, id := range ids {
		n := 0
		if strings.EqualFold(id, first) {
			n = 1
		} else if rest, ok := strings.CutPrefix(strings.ToLower(id), strings.ToLower(first)+"_"); ok {
			if v, err := strconv.Atoi(rest); err == nil && v > 1 && v <= maxSession && strconv.Itoa(v) == rest {
				n = v
			}
		}
		top = max(top, n)
	}
	return top
}

func (b *Bridge) idleReset() time.Duration {
	if b.cfg.IdleReset == 0 {
		return DefaultIdleReset
	}
	return b.cfg.IdleReset
}

// renew starts the chat's next conversation (/new), unless a run goes.
func (b *Bridge) renew(ctx context.Context, chatID string) {
	b.mu.Lock()
	busy := b.chats[chatID] != nil
	b.mu.Unlock()
	if busy {
		b.say(ctx, chatID, b.tr(chatID, mBusyNew))
		return
	}
	b.reconcile(ctx, chatID)
	b.mu.Lock()
	s := b.sessions[chatID]
	s.N, s.Last = max(s.N, 1)+1, b.now()
	b.sessions[chatID] = s
	b.mu.Unlock()
	b.persist()
	b.say(ctx, chatID, b.tr(chatID, mNew))
}

// current is the chat's conversation now.
func (b *Bridge) current(chatID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Conversation(b.cfg.Channel.Name(), chatID, max(b.sessions[chatID].N, 1))
}
