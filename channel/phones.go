// phones.go: members let in by the phone number they share
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/agilira/go-errors"
)

// PhoneAsker is a Channel that can ask a user for their phone number,
// with the app's own button for sharing it, and that sets Message.Phone
// only to the sender's own number, as the app vouches for it. The Bridge
// needs it for Config.MemberPhones. AskPhone is called as Send is.
type PhoneAsker interface {
	AskPhone(ctx context.Context, chat, text, button string) error
}

// maxPhonesFile bounds the shared phones file read back.
const maxPhonesFile = 4 << 20

// normalPhone is a phone number as digits, with its country code: "+39
// 333 123 4567", "0039 333-1234567" and "393331234567" are all
// "393331234567". ok is false for anything that is not 8-15 digits
// (E.164) once spaces, dashes, dots and brackets are taken out. Marked
// (the host's list) wants the country code marked with + or 00: without
// it, a number written as at home would never match.
func normalPhone(s string, marked bool) (string, bool) {
	if marked && strings.Contains(s, "(0)") {
		return "", false // "+44 (0)20": the 0 is dialled at home only
	}
	s, plus := countryCode(strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '.', '(', ')':
			return -1
		}
		return r
	}, s))
	if (!plus && marked) || !e164(s) {
		return "", false
	}
	return s, true
}

// e164: 8-15 digits, not starting with 0, as a number with its country
// code is.
func e164(s string) bool {
	return len(s) >= 8 && len(s) <= 15 && strings.Trim(s, "0123456789") == "" && s[0] != '0'
}

// countryCode takes away the + or 00 that marks a country code, saying
// whether there was one.
func countryCode(s string) (string, bool) {
	if rest, ok := strings.CutPrefix(s, "+"); ok {
		return rest, true
	}
	return strings.CutPrefix(s, "00")
}

// phoneSet reads the member phones of the host; an error names the
// first that is not a phone number with its country code.
func phoneSet(list []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, p := range list {
		if strings.TrimSpace(p) == "" {
			continue
		}
		n, ok := normalPhone(p, true)
		if !ok {
			return nil, errors.New(ErrConfig, "not a phone number with its country code: "+p)
		}
		out[n] = true
	}
	return out, nil
}

// loadShared reads the senders who shared a listed phone, by sender ID;
// those whose phone is no longer listed are forgotten (pruned).
func loadShared(path string, listed map[string]bool) (out map[string]string, pruned bool, err error) {
	out = map[string]string{}
	data, err := readState(path, maxPhonesFile, "phones")
	switch {
	case err != nil:
		return nil, false, err
	case data == nil:
		return out, false, nil
	case json.Unmarshal(data, &out) != nil || out == nil:
		return nil, false, errors.New(ErrConfig, "the phones file is damaged: "+path)
	}
	for from, phone := range out {
		if !listed[phone] {
			delete(out, from)
			pruned = true
		}
	}
	return out, pruned, nil
}

// sharesPhone: m shares its sender's phone in their own chat, and the
// host lets members in by phone.
func (b *Bridge) sharesPhone(m Message) bool {
	return m.Phone != "" && !m.Group && m.From != "" && len(b.people().phones) > 0
}

// phoneMember: from shared a phone the host lists.
func (b *Bridge) phoneMember(from string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.shared[from]
	return ok && b.people().phones[p]
}

// askPhone asks a sender in their own chat for their phone, when the host
// lets members in by phone; at most MaxPerHour times an hour (30 when
// there is no bound): a stranger writing on and on is not answered on
// and on.
func (b *Bridge) askPhone(ctx context.Context, m Message, text msg) {
	if m.Group || len(b.people().phones) == 0 || !b.within(m.From, stranger) {
		return
	}
	b.mu.Lock()
	b.queue(ctx, m.Chat, outgoing{out: Out{Text: b.tr(m.Chat, text)}, askPhone: true})
	b.mu.Unlock()
}

// sharedPhone takes the phone a sender shared in their own chat: a
// listed one makes them a member, kept in Config.SharedPhones.
func (b *Bridge) sharedPhone(ctx context.Context, m Message) {
	if !b.within(m.From, stranger) {
		b.report(Event{Kind: Limited, Chat: m.Chat, From: m.From})
		return
	}
	phone, ok := normalPhone(m.Phone, false)
	if !ok || !b.people().phones[phone] {
		b.report(Event{Kind: Refused, Chat: m.Chat, From: m.From})
		b.say(ctx, m.Chat, b.tr(m.Chat, mPhoneNo))
		return
	}
	b.mu.Lock()
	b.shared[m.From] = phone
	b.mu.Unlock()
	b.persistShared()
	b.say(ctx, m.Chat, b.tr(m.Chat, mPhoneYes))
}

// persistShared writes the shared phones, as persist the sessions.
func (b *Bridge) persistShared() {
	if b.cfg.SharedPhones == "" {
		return
	}
	b.saving.Lock()
	defer b.saving.Unlock()
	b.mu.Lock()
	data, err := json.Marshal(b.shared)
	b.mu.Unlock()
	if err == nil {
		err = writeFile(b.cfg.SharedPhones, data)
	}
	if err != nil {
		b.report(Event{Kind: PhonesFailed, Err: err})
	}
}
