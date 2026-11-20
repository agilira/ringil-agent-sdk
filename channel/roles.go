// roles.go: owners, members, the public, and what each may do
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"strings"
	"time"
)

// role is who a sender is to the Bridge.
type role int

const (
	stranger role = iota
	public
	member
	owner
)

// memberCheck is an answer of IsMember, and when it was given.
type memberCheck struct {
	member bool
	at     time.Time
}

func idSet(ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}

// roleOf is a sender's role: listed owner or member, a member by a
// listed phone they shared or by IsMember, the public when there is a public profile, else a stranger.
func (b *Bridge) roleOf(ctx context.Context, from string) role {
	switch {
	case from == "":
		return stranger
	case b.owners[from]:
		return owner
	case b.members[from] || b.phoneMember(from) || b.isMember(ctx, from):
		return member
	case b.cfg.PublicProfile != "":
		return public
	}
	return stranger
}

// known: from is an owner or a member as far as is known now, without
// asking IsMember (what listening does: it must not hold up the chats
// for every stranger in a group). A member by IsMember counts once a
// message of theirs was answered.
func (b *Bridge) known(from string) bool {
	if from == "" {
		return false
	}
	if b.owners[from] || b.members[from] || b.phoneMember(from) {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.checked[from]
	return ok && c.member && b.now().Sub(c.at) < b.memberCheck()
}

// isMember asks IsMember, keeping its answers for MemberCheck.
func (b *Bridge) isMember(ctx context.Context, from string) bool {
	if b.cfg.IsMember == nil {
		return false
	}
	now := b.now()
	b.mu.Lock()
	c, ok := b.checked[from]
	b.mu.Unlock()
	if ok && now.Sub(c.at) < b.memberCheck() {
		return c.member
	}
	ask, cancel := context.WithTimeout(ctx, memberAskTimeout)
	defer cancel()
	yes, err := b.cfg.IsMember(ask, from)
	if err != nil {
		b.report(Event{Kind: MemberCheckFailed, From: from, Err: err})
		// Kept briefly as a no: a check that fails must not be asked
		// again at every message, holding up every chat.
		now = now.Add(memberRetry - b.memberCheck())
	}
	b.mu.Lock()
	if len(b.checked) >= maxTracked {
		clear(b.checked) // answers only kept to spare questions
	}
	b.checked[from] = memberCheck{member: yes && err == nil, at: now}
	b.mu.Unlock()
	return yes && err == nil
}

// A membership check waits at most memberAskTimeout; a failed one is
// asked again after memberRetry.
const (
	memberAskTimeout = 10 * time.Second
	memberRetry      = time.Minute
)

func (b *Bridge) memberCheck() time.Duration {
	if b.cfg.MemberCheck > 0 {
		return b.cfg.MemberCheck
	}
	return 10 * time.Minute
}

// profileFor is the profile a run talks with: an owner's own chat gets
// Profile, members theirs, the public its own. A group gets the profile
// of the least trusted who may talk there, whoever writes: its single
// conversation, and every answer, is read by all of them.
func (b *Bridge) profileFor(r role, group bool) string {
	switch {
	case group:
		return b.groupProfile()
	case r == owner:
		return b.cfg.Profile
	case r == member:
		return b.cfg.MemberProfile
	}
	return b.cfg.PublicProfile
}

// groupProfile is PublicProfile when there is one, else MemberProfile;
// "" when neither: then the Bridge does not talk in groups.
func (b *Bridge) groupProfile() string {
	if b.cfg.PublicProfile != "" {
		return b.cfg.PublicProfile
	}
	return b.cfg.MemberProfile
}

// within records a message of a member, public or stranger sender and
// says whether it is within MaxPerHour; strangers (asked for their phone)
// always have a bound, 30 when MaxPerHour has none.
func (b *Bridge) within(from string, r role) bool {
	limit := b.limit(r)
	if r == owner || limit < 0 {
		return true
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.recent[from][:0:0]
	for _, t := range b.recent[from] {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		b.recent[from] = kept
		return false
	}
	if !b.room(from, now) {
		return r == member // a flood: no one new is tracked
	}
	b.recent[from] = append(kept, now)
	return true
}

// limit is the messages an hour of a sender of role r; negative, none.
func (b *Bridge) limit(r role) int {
	limit := b.cfg.MaxPerHour
	if limit == 0 || (limit < 0 && r == stranger) {
		limit = 30
	}
	return limit
}

// room says whether from can be tracked: already is, or there is room,
// made by forgetting the quiet senders when needed; b.mu held.
func (b *Bridge) room(from string, now time.Time) bool {
	if _, known := b.recent[from]; known || len(b.recent) < maxTracked {
		return true
	}
	b.forgetQuiet(now)
	return len(b.recent) < maxTracked
}

// maxTracked bounds the senders the Bridge keeps track of: a public bot
// hears from anyone.
const maxTracked = 10000

// forgetQuiet drops the senders without a message in the last hour;
// b.mu held.
func (b *Bridge) forgetQuiet(now time.Time) {
	for from, times := range b.recent {
		if len(times) == 0 || now.Sub(times[len(times)-1]) >= time.Hour {
			delete(b.recent, from)
		}
	}
}
