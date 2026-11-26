// conversations.go: the stored conversations, as an app lists and shows them
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Bounds of a page of history.
const (
	defaultHistory = 200
	maxHistory     = 1000
)

// ConversationInfo is a stored conversation, as a list of chats shows it.
type ConversationInfo struct {
	ID      string    `json:"id"`
	Title   string    `json:"title,omitzero"`
	Updated time.Time `json:"updated"`
	Size    int64     `json:"size"`
	// Running: a run of it is going; its messages are read from its events
	// until it ends.
	Running bool `json:"running,omitzero"`
}

// Conversations lists the conversations the server keeps, the latest
// first; none without a store.
func (s *Server) Conversations(ctx context.Context) ([]ConversationInfo, error) {
	if s.cfg.Store == nil {
		return nil, nil
	}
	infos, err := s.cfg.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ConversationInfo, 0, len(infos))
	for _, i := range infos {
		out = append(out, ConversationInfo{ID: i.ID, Title: i.Title, Updated: i.Updated, Size: i.Size, Running: s.running(i.ID)})
	}
	return out, nil
}

// running says whether a run of the conversation is going.
func (s *Server) running(conversation string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.runs {
		if strings.EqualFold(j.conversation, conversation) && !j.finished() {
			return true
		}
	}
	return false
}

// History is a page of a conversation's messages.
type History struct {
	// From is the index of the first message given; Total how many the
	// conversation holds.
	From     int              `json:"from"`
	Total    int              `json:"total"`
	Messages []HistoryMessage `json:"messages"`
}

// HistoryMessage is one message as an app shows it: its text, the calls a
// model made, a tool's result, what was attached (named, not sent again).
// The model's reasoning and the context the host added are left out.
type HistoryMessage struct {
	Index       int            `json:"index"`
	Role        agent.Role     `json:"role"`
	Text        string         `json:"text,omitzero"`
	Calls       []wireCall     `json:"calls,omitzero"`
	Result      *HistoryResult `json:"result,omitzero"`
	Attachments []Attachment   `json:"attachments,omitzero"`
}

// HistoryResult is a tool's result in the history.
type HistoryResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitzero"`
}

// Attachment names an image or a document of a message.
type Attachment struct {
	Type  agent.PartType `json:"type"`
	MIME  string         `json:"mime"`
	Bytes int            `json:"bytes"`
}

// History returns limit messages of a conversation from index from (from
// < 0: the last limit). A conversation with a run going is busy: its
// messages come with the run's events.
func (s *Server) History(ctx context.Context, id string, from, limit int) (History, error) {
	if s.cfg.Store == nil {
		return History{}, fail(ErrNotFound, "the server keeps no conversations")
	}
	if err := agent.ValidateSessionID(id); err != nil {
		return History{}, err
	}
	if !s.exists(ctx, id) { // Open would create it
		return History{}, fail(ErrNotFound, "no conversation "+id)
	}
	if s.running(id) {
		return History{}, fail(ErrBusy, "the conversation has a run going")
	}
	sess, err := s.cfg.Store.Open(ctx, id)
	if err != nil {
		return History{}, err
	}
	defer func() { _ = sess.Close() }()
	msgs := sess.Messages()
	from, end := page(len(msgs), from, limit)
	out := History{From: from, Total: len(msgs), Messages: make([]HistoryMessage, 0, end-from)}
	for i := from; i < end; i++ {
		out.Messages = append(out.Messages, historyMessage(i, msgs[i]))
	}
	return out, nil
}

// page is the range of a page of limit of total messages from from
// (from < 0: the last page).
func page(total, from, limit int) (int, int) {
	if limit <= 0 || limit > maxHistory {
		limit = defaultHistory
	}
	if from < 0 {
		from = max(total-limit, 0)
	}
	from = min(from, total)
	return from, min(from+limit, total)
}

func historyMessage(i int, m agent.Message) HistoryMessage {
	h := HistoryMessage{Index: i, Role: m.Role, Text: m.Text(), Attachments: attachments(m.Parts)}
	for _, c := range m.Calls {
		h.Calls = append(h.Calls, wireCall{ID: c.ID, Name: c.Name, Input: c.Input})
	}
	if r := m.Result; r != nil {
		h.Result = &HistoryResult{CallID: r.CallID, Content: r.Content, IsError: r.IsError}
		h.Attachments = append(h.Attachments, attachments(r.Images)...)
	}
	return h
}

func attachments(parts []agent.Part) []Attachment {
	var out []Attachment
	for _, p := range parts {
		if p.Type == agent.PartImage || p.Type == agent.PartDocument {
			out = append(out, Attachment{Type: p.Type, MIME: p.MIME, Bytes: len(p.Data)})
		}
	}
	return out
}

// Rename sets a conversation's title; empty removes it.
func (s *Server) Rename(ctx context.Context, id, title string) error {
	if err := agent.ValidateSessionID(id); err != nil {
		return err
	}
	ts, ok := s.cfg.Store.(agent.TitleStore)
	if !ok {
		return fail(ErrConfig, "the store keeps no titles")
	}
	if !s.exists(ctx, id) {
		return fail(ErrNotFound, "no conversation "+id)
	}
	return ts.SetTitle(ctx, id, title)
}

// DeleteConversation removes a stored conversation that has no run going.
func (s *Server) DeleteConversation(ctx context.Context, id string) error {
	if err := agent.ValidateSessionID(id); err != nil {
		return err
	}
	if s.cfg.Store == nil || !s.exists(ctx, id) {
		return fail(ErrNotFound, "no conversation "+id)
	}
	if s.running(id) {
		return fail(ErrBusy, "the conversation has a run going")
	}
	return s.cfg.Store.Delete(ctx, id)
}

func (s *Server) exists(ctx context.Context, id string) bool {
	if s.cfg.Store == nil {
		return false
	}
	infos, err := s.cfg.Store.List(ctx)
	if err != nil {
		return false
	}
	for _, i := range infos {
		if i.ID == id {
			return true
		}
	}
	return false
}

// titleFirst gives a new conversation its title from its first message:
// the words the user wrote, or what they attached.
func (s *Server) titleFirst(ctx context.Context, id string, conv agent.Conversation, input agent.Message) {
	ts, ok := s.cfg.Store.(agent.TitleStore)
	if !ok || id == "" || len(conv.Messages()) > 0 {
		return
	}
	title := input.Text()
	if strings.TrimSpace(title) == "" {
		switch a := attachments(input.Parts); {
		case len(a) == 0:
			return
		case a[0].Type == agent.PartDocument:
			title = "PDF document"
		default:
			title = "Image"
		}
	}
	_ = ts.SetTitle(ctx, id, title) // a title is not worth failing the run
}

// HTTP handlers.

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	list, err := s.Conversations(r.Context())
	if err != nil {
		failure(w, status(err), err)
		return
	}
	respond(w, http.StatusOK, list)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	from, limit := -1, 0
	q := r.URL.Query()
	if v := q.Get("from"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			failure(w, http.StatusBadRequest, fail(ErrInput, "from is a message index, from 0"))
			return
		}
		from = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			failure(w, http.StatusBadRequest, fail(ErrInput, "limit is a number of messages, from 1"))
			return
		}
		limit = n
	}
	h, err := s.History(r.Context(), r.PathValue("id"), from, limit)
	if err != nil {
		failure(w, status(err), err)
		return
	}
	respond(w, http.StatusOK, h)
}

type renameIn struct {
	Title string `json:"title"`
}

func (s *Server) rename(w http.ResponseWriter, r *http.Request) {
	var in renameIn
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.Rename(r.Context(), r.PathValue("id"), in.Title); err != nil {
		failure(w, status(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteConversation(w http.ResponseWriter, r *http.Request) {
	if err := s.DeleteConversation(r.Context(), r.PathValue("id")); err != nil {
		failure(w, status(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// storeStatus maps the store's errors.
func storeStatus(err error) (int, bool) {
	switch {
	case errors.HasCode(err, agent.ErrSessionBusy):
		return http.StatusConflict, true
	case errors.HasCode(err, agent.ErrSessionInput):
		return http.StatusBadRequest, true
	}
	return 0, false
}
