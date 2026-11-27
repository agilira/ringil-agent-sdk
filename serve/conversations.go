// conversations.go: the stored conversations, as an app lists and shows them
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/artifact"
)

// Bounds of a page of history.
const (
	defaultHistory = 200
	maxHistory     = 1000
)

// ConversationInfo is a stored conversation, as a list of chats shows it.
type ConversationInfo struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitzero"`
	Pinned bool   `json:"pinned,omitzero"`
	// Project is the project it works on, the host's name for it.
	Project string `json:"project,omitzero"`
	// Profile is the agent that answered it last.
	Profile string    `json:"profile,omitzero"`
	Updated time.Time `json:"updated"`
	Size    int64     `json:"size"`
	// Running: a run of it is going; its messages are read from its events
	// until it ends.
	Running bool `json:"running,omitzero"`
}

// Conversations lists the conversations the server keeps: the pinned
// first, then the others, each part the latest first; none without a
// store.
func (s *Server) Conversations(ctx context.Context) ([]ConversationInfo, error) {
	if s.cfg.Store == nil {
		return []ConversationInfo{}, nil
	}
	infos, err := s.cfg.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ConversationInfo, 0, len(infos))
	for _, i := range infos {
		out = append(out, ConversationInfo{ID: i.ID, Title: i.Title, Pinned: i.Pinned, Profile: i.Profile, Project: i.Project, Updated: i.Updated, Size: i.Size, Running: s.running(i.ID)})
	}
	slices.SortStableFunc(out, func(a, b ConversationInfo) int {
		switch {
		case a.Pinned == b.Pinned:
			return b.Updated.Compare(a.Updated)
		case a.Pinned:
			return -1
		}
		return 1
	})
	return out, nil
}

// running says whether a run of the conversation is going.
func (s *Server) running(conversation string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runningLocked(conversation)
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
	release, err := s.hold(id, holdRead)
	if err != nil {
		return History{}, err
	}
	defer release()
	if !s.exists(ctx, id) { // Open would create it
		return History{}, fail(ErrNotFound, "no conversation "+id)
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

// ConversationChange is what an app changes of a conversation's labels:
// a field left nil stays as it is.
type ConversationChange struct {
	// Title, empty, removes it.
	Title  *string `json:"title,omitempty"`
	Pinned *bool   `json:"pinned,omitempty"`
	// Project moves it to a project (the host's name), or out of one ("").
	Project *string `json:"project,omitempty"`
}

// ChangeConversation changes a conversation's title or pin.
func (s *Server) ChangeConversation(ctx context.Context, id string, c ConversationChange) error {
	if err := agent.ValidateSessionID(id); err != nil {
		return err
	}
	if err := c.check(); err != nil {
		return err
	}
	if c.Project != nil {
		if err := s.checkProject(ctx, *c.Project); err != nil {
			return err
		}
	}
	ls, ok := s.cfg.Store.(agent.LabelStore)
	if !ok {
		return fail(ErrNotFound, "the server keeps no conversation labels")
	}
	release, err := s.hold(id, holdLabel)
	if err != nil {
		return err
	}
	defer release()
	if !s.exists(ctx, id) {
		return fail(ErrNotFound, "no conversation "+id)
	}
	return ls.Label(ctx, id, c.apply)
}

// check: c changes something, and its project is a name.
func (c ConversationChange) check() error {
	switch {
	case c.Title == nil && c.Pinned == nil && c.Project == nil:
		return fail(ErrInput, "nothing to change: give a title, pinned or project")
	case c.Project != nil && (agent.Labels{Project: *c.Project}).Check() != nil:
		return fail(ErrInput, "bad project name")
	}
	return nil
}

// apply makes the change on l.
func (c ConversationChange) apply(l *agent.Labels) {
	if c.Title != nil {
		l.Title = *c.Title
	}
	if c.Pinned != nil {
		l.Pinned = *c.Pinned
	}
	if c.Project != nil {
		l.Project = *c.Project
	}
}

// DeleteConversation removes a stored conversation that has no run going.
func (s *Server) DeleteConversation(ctx context.Context, id string) error {
	if err := agent.ValidateSessionID(id); err != nil {
		return err
	}
	if s.cfg.Store == nil {
		return fail(ErrNotFound, "no conversation "+id)
	}
	release, err := s.hold(id, holdExclude)
	if err != nil {
		return err
	}
	defer release()
	if !s.exists(ctx, id) {
		return fail(ErrNotFound, "no conversation "+id)
	}
	if err := s.cfg.Store.Delete(ctx, id); err != nil {
		return err
	}
	if s.cfg.Artifacts != nil && artifact.ValidScope(id) {
		// Its artifacts go with it; the conversation is gone whatever
		// happens here, and a scope left over is empty to the app.
		_ = s.cfg.Artifacts.DeleteScope(ctx, id)
	}
	return nil
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

// labelRun marks a conversation with the profile of the run starting on
// it, and gives a new one its title from its first message: the words
// the user wrote, or what they attached.
func (s *Server) labelRun(ctx context.Context, id, profile, project string, conv agent.Conversation, input agent.Message) {
	ls, ok := s.cfg.Store.(agent.LabelStore)
	if !ok || id == "" {
		return
	}
	title := ""
	if len(conv.Messages()) == 0 {
		title = firstTitle(input)
	}
	_ = ls.Label(ctx, id, func(l *agent.Labels) { // labels are not worth failing the run
		l.Profile = profile
		if project != "" {
			l.Project = project
		}
		if l.Title == "" {
			l.Title = title
		}
	})
}

// projectOf is the project of a run: the one asked, which must be the
// conversation's when it has one; else the conversation's.
func (s *Server) projectOf(ctx context.Context, conversation, asked string) (string, error) {
	if err := s.checkProject(ctx, asked); err != nil {
		return "", err
	}
	if conversation == "" || s.cfg.Store == nil {
		return asked, nil
	}
	has, err := s.conversationProject(ctx, conversation)
	switch {
	case err != nil:
		return "", err
	case asked == "":
		return s.checkedProject(ctx, has)
	case has != "" && has != asked:
		return "", fail(ErrInput, "the conversation belongs to another project")
	}
	return asked, nil
}

func (s *Server) checkedProject(ctx context.Context, id string) (string, error) {
	if err := s.checkProject(ctx, id); err != nil {
		return "", err
	}
	return id, nil
}

// checkProject: a project named is a name, and the host takes it.
func (s *Server) checkProject(ctx context.Context, project string) error {
	if project == "" {
		return nil
	}
	if (agent.Labels{Project: project}).Check() != nil {
		return fail(ErrInput, "bad project name")
	}
	if s.cfg.CheckProject != nil {
		if err := s.cfg.CheckProject(ctx, project); err != nil {
			return fail(ErrInput, userMessage(err))
		}
	}
	return nil
}

// conversationProject is the project a stored conversation is on, "" for
// none, a conversation not there yet, or a store without labels.
func (s *Server) conversationProject(ctx context.Context, conversation string) (string, error) {
	ls, ok := s.cfg.Store.(agent.LabelStore)
	if !ok {
		return "", nil
	}
	l, err := ls.Labels(ctx, conversation)
	return l.Project, err
}

// firstTitle is a new conversation's title from its first message.
func firstTitle(input agent.Message) string {
	if title := input.Text(); strings.TrimSpace(title) != "" {
		return title
	}
	switch a := attachments(input.Parts); {
	case len(a) == 0:
		return ""
	case a[0].Type == agent.PartDocument:
		return "PDF document"
	}
	return "Image"
}

// HTTP handlers.

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	list, err := s.Conversations(r.Context())
	if err != nil {
		failure(w, status(err), err)
		return
	}
	if p, ok := r.URL.Query()["project"]; ok { // ?project= : those of none
		kept := list[:0]
		for _, c := range list {
			if c.Project == p[0] {
				kept = append(kept, c)
			}
		}
		list = kept
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

func (s *Server) changeConversation(w http.ResponseWriter, r *http.Request) {
	var in ConversationChange
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.ChangeConversation(r.Context(), r.PathValue("id"), in); err != nil {
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
