// people.go: who a Bridge talks with, changed while it runs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import "github.com/agilira/go-errors"

// People is who a Bridge talks with, as in Config: its owners, members
// (listed, or by a phone they share) and their profile, the public's
// profile, how many messages an hour a member or the public has
// answered, and whether the owners' runs ask before tool calls.
type People struct {
	Owners        []string
	Members       []string
	MemberProfile string
	MemberPhones  []string
	PublicProfile string
	MaxPerHour    int
	NoApprovals   bool
}

// PeopleOf is the People of a Config.
func PeopleOf(cfg Config) People {
	return People{Owners: cfg.Owners, Members: cfg.Members, MemberProfile: cfg.MemberProfile,
		MemberPhones: cfg.MemberPhones, PublicProfile: cfg.PublicProfile, MaxPerHour: cfg.MaxPerHour,
		NoApprovals: cfg.NoApprovals}
}

// people is People checked and indexed.
type people struct {
	owners, members, phones      map[string]bool
	memberProfile, publicProfile string
	maxPerHour                   int
	noApprovals                  bool
}

// peopleOf checks p against the Bridge's channel and indexes it.
func peopleOf(cfg Config, p People) (*people, error) {
	phones, err := phoneSet(p.MemberPhones)
	if err != nil {
		return nil, err
	}
	members := idSet(p.Members)
	if _, ok := cfg.Channel.(PhoneAsker); len(phones) > 0 && !ok {
		return nil, errors.New(ErrConfig, "this channel cannot ask for phone numbers: no MemberPhones")
	}
	if (len(members) > 0 || len(phones) > 0 || cfg.IsMember != nil) && p.MemberProfile == "" {
		return nil, errors.New(ErrConfig, "members need a MemberProfile: the tools they may use")
	}
	return &people{owners: idSet(p.Owners), members: members, phones: phones,
		memberProfile: p.MemberProfile, publicProfile: p.PublicProfile, maxPerHour: p.MaxPerHour,
		noApprovals: p.NoApprovals}, nil
}

// SetPeople changes who the Bridge talks with, from the next message: a
// run going keeps the role it started with. A sender whose phone is taken
// off MemberPhones is no longer a member, also on disk. An error leaves
// them as they were.
func (b *Bridge) SetPeople(p People) error {
	who, err := peopleOf(b.cfg, p)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.who.Store(who)
	pruned := false
	for from, phone := range b.shared {
		if !who.phones[phone] {
			delete(b.shared, from)
			pruned = true
		}
	}
	b.mu.Unlock()
	if pruned {
		b.persistShared()
	}
	return nil
}

func (b *Bridge) people() *people { return b.who.Load() }
