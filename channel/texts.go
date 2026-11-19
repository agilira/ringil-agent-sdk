// texts.go: what the Bridge says, in the chat's language
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"regexp"
	"strings"

	"github.com/agilira/go-errors"
)

// Languages the Bridge speaks (Config.Language).
const (
	English = "en"
	Italian = "it"
)

// msg names a text of the Bridge; its {} are filled in order.
type msg string

const (
	mHello         msg = "hello"
	mHelloPhone    msg = "hello_phone"
	mAskPhone      msg = "ask_phone"
	mPhoneButton   msg = "phone_button"
	mPhoneNo       msg = "phone_no"
	mPhoneYes      msg = "phone_yes"
	mTooMany       msg = "too_many"
	mUnreadable    msg = "unreadable"
	mOwnerOnly     msg = "owner_only"
	mNothingStop   msg = "nothing_to_stop"
	mNoGroups      msg = "no_groups"
	mBusy          msg = "busy"
	mBusyNew       msg = "busy_new"
	mQuiet         msg = "quiet"
	mNew           msg = "new"
	mStopped       msg = "stopped"
	mDone          msg = "done"
	mCannotAnswer  msg = "cannot_answer"
	mMaxFiles      msg = "max_files"
	mNoVoiceIn     msg = "no_voice_in"
	mVoiceUnheard  msg = "voice_unheard"
	mImages        msg = "images"
	mNoDocuments   msg = "no_documents"
	mNotKept       msg = "not_kept"
	mNotFetched    msg = "not_fetched"
	mTooLarge      msg = "too_large"
	mTheFile       msg = "the_file"
	mNoVoiceOut    msg = "no_voice_out"
	mVoiceOn       msg = "voice_on"
	mVoiceOff      msg = "voice_off"
	mYes           msg = "yes"
	mAlways        msg = "always"
	mShowAll       msg = "show_all"
	mMoreLines     msg = "more_lines"
	mWhole         msg = "whole"
	mNo            msg = "no"
	mAllowed       msg = "allowed"
	mRefused       msg = "refused"
	mOwnerApproves msg = "owner_approves"
	mOwnerAnswers  msg = "owner_answers"
	mClosed        msg = "closed"
	mSeveral       msg = "several"
	mWorkerAsks    msg = "worker_asks"
	mInGroup       msg = "in_group"
	mAskTool       msg = "ask_tool"
	mAskWrite      msg = "ask_write"
	mAskEdit       msg = "ask_edit"
	mAskEditAll    msg = "ask_edit_all"
	mAskProtected  msg = "ask_protected"
	mAskCommand    msg = "ask_command"
	mOutside       msg = "outside"
	mAgentSays     msg = "agent_says"
	mAskDomain     msg = "ask_domain"
)

var texts = map[string]speech{
	English: {
		mHello:         "Hello. Write to me and I will answer. /new starts a new conversation, /stop stops what I am doing, /voice and /text choose how I answer voice messages.",
		mHelloPhone:    "If you work here, share your phone number with the button below.",
		mAskPhone:      "To talk with me, share your phone number with the button below.",
		mPhoneButton:   "Share my phone number",
		mPhoneNo:       "This number is not among those allowed here.",
		mPhoneYes:      "Thank you: you can write to me now.",
		mTooMany:       "You have sent many messages this hour; please wait a little.",
		mUnreadable:    "I cannot read this kind of message.",
		mOwnerOnly:     "Only an owner can do that here.",
		mNothingStop:   "Nothing to stop.",
		mNoGroups:      "I am not set up to talk in groups.",
		mBusy:          "I am still working on your last message; send /stop to stop it.",
		mBusyNew:       "I am still working on your last message; send /stop first.",
		mQuiet:         "This chat was quiet for a while, so this is a new conversation: I no longer see the earlier messages.",
		mNew:           "New conversation: I no longer see the earlier messages. What I was asked to remember stays.",
		mStopped:       "Stopped.",
		mDone:          "Done.",
		mCannotAnswer:  "I cannot answer now.",
		mMaxFiles:      "Send at most 10 files at a time.",
		mNoVoiceIn:     "I cannot listen to voice messages here; please write.",
		mVoiceUnheard:  "I could not understand the voice message; please write.",
		mImages:        "I can take images in JPEG, PNG, GIF or WebP up to 5 MB.",
		mNoDocuments:   "I cannot take documents here.",
		mNotKept:       "I could not keep {}.",
		mNotFetched:    "I could not download {}.",
		mTooLarge:      "{} is too large: up to {} MB.",
		mTheFile:       "the file",
		mNoVoiceOut:    "I cannot answer with voice here.",
		mVoiceOn:       "I will answer your voice messages with voice.",
		mVoiceOff:      "I will answer your voice messages in text.",
		mYes:           "Yes",
		mAlways:        "Yes, always",
		mShowAll:       "Show all",
		mMoreLines:     "… {} more lines not shown: Show all to read them.",
		mWhole:         "All of it:",
		mNo:            "No",
		mAllowed:       "Allowed.",
		mRefused:       "Refused.",
		mOwnerApproves: "Refused: only an owner can allow this.",
		mOwnerAnswers:  "Only an owner can answer that.",
		mClosed:        "That question is closed.",
		mSeveral:       "Several questions are open: answer each with its buttons.",
		mWorkerAsks:    "Worker {} asks. ",
		mInGroup:       " (an owner answers)",
		mAskTool:       "May I use {}?",
		mAskWrite:      "May I write this to {}?",
		mAskEdit:       "May I change {}? Lines with - go, lines with + come:",
		mAskEditAll:    "May I change {}, everywhere the text appears? Lines with - go, lines with + come:",
		mAskProtected:  "May I write {}? It is a protected file (rules, settings or .git).",
		mAskCommand:    "May I run this command?",
		mOutside:       "It runs outside the sandbox, with your rights.",
		mAgentSays:     "The agent says: {}",
		mAskDomain:     "A command wants to reach {}. May it, for this conversation?",
	},
	Italian: {
		mHello:         "Ciao. Scrivimi e ti rispondo. /new apre una nuova conversazione, /stop ferma quello che sto facendo, /voice e /text scelgono come rispondo ai vocali.",
		mHelloPhone:    "Se lavori qui, condividi il tuo numero con il bottone qui sotto.",
		mAskPhone:      "Per parlare con me, condividi il tuo numero con il bottone qui sotto.",
		mPhoneButton:   "Condividi il mio numero",
		mPhoneNo:       "Questo numero non è tra quelli ammessi.",
		mPhoneYes:      "Grazie: ora puoi scrivermi.",
		mTooMany:       "Hai mandato molti messaggi in quest'ora; aspetta un po'.",
		mUnreadable:    "Non riesco a leggere questo tipo di messaggio.",
		mOwnerOnly:     "Qui può farlo solo un proprietario.",
		mNothingStop:   "Non c'è niente da fermare.",
		mNoGroups:      "Non sono configurato per parlare nei gruppi.",
		mBusy:          "Sto ancora lavorando al tuo ultimo messaggio; manda /stop per fermarmi.",
		mBusyNew:       "Sto ancora lavorando al tuo ultimo messaggio; prima manda /stop.",
		mQuiet:         "La chat è rimasta ferma a lungo, quindi questa è una nuova conversazione: non vedo più i messaggi di prima.",
		mNew:           "Nuova conversazione: non vedo più i messaggi di prima. Quello che mi hai chiesto di ricordare resta.",
		mStopped:       "Fermato.",
		mDone:          "Fatto.",
		mCannotAnswer:  "Ora non riesco a rispondere.",
		mMaxFiles:      "Manda al massimo 10 file alla volta.",
		mNoVoiceIn:     "Qui non posso ascoltare i vocali; scrivimi.",
		mVoiceUnheard:  "Non ho capito il vocale; scrivimi.",
		mImages:        "Prendo immagini JPEG, PNG, GIF o WebP fino a 5 MB.",
		mNoDocuments:   "Qui non posso prendere documenti.",
		mNotKept:       "Non sono riuscito a tenere {}.",
		mNotFetched:    "Non sono riuscito a scaricare {}.",
		mTooLarge:      "{} è troppo grande: al massimo {} MB.",
		mTheFile:       "il file",
		mNoVoiceOut:    "Qui non posso rispondere con la voce.",
		mVoiceOn:       "Risponderò ai tuoi vocali con la voce.",
		mVoiceOff:      "Risponderò ai tuoi vocali per iscritto.",
		mYes:           "Sì",
		mAlways:        "Sì, sempre",
		mShowAll:       "Mostra tutto",
		mMoreLines:     "… altre {} righe non mostrate: Mostra tutto per leggerle.",
		mWhole:         "Tutto il contenuto:",
		mNo:            "No",
		mAllowed:       "Permesso.",
		mRefused:       "Rifiutato.",
		mOwnerApproves: "Rifiutato: solo un proprietario può permetterlo.",
		mOwnerAnswers:  "Solo un proprietario può rispondere.",
		mClosed:        "Questa domanda è chiusa.",
		mSeveral:       "Ci sono più domande aperte: rispondi a ciascuna con i suoi bottoni.",
		mWorkerAsks:    "Lo chiede il worker {}. ",
		mInGroup:       " (risponde un proprietario)",
		mAskTool:       "Posso usare {}?",
		mAskWrite:      "Posso scrivere questo in {}?",
		mAskEdit:       "Posso modificare {}? Le righe con - escono, quelle con + entrano:",
		mAskEditAll:    "Posso modificare {}, ovunque compaia il testo? Le righe con - escono, quelle con + entrano:",
		mAskProtected:  "Posso scrivere {}? È un file protetto (regole, impostazioni o .git).",
		mAskCommand:    "Posso eseguire questo comando?",
		mOutside:       "Gira fuori dalla sandbox, con i tuoi permessi.",
		mAgentSays:     "L'agente dice: {}",
		mAskDomain:     "Un comando vuole raggiungere {}. Glielo permetto, per questa conversazione?",
	},
}

// Texts are the Bridge's own texts in English, by name, for a host to
// translate into Config.Languages: a text's {} are filled in order.
func Texts() map[string]string {
	out := map[string]string{}
	for m, s := range texts[English] {
		out[string(m)] = s
	}
	return out
}

// languages are the languages the Bridge speaks: its own, and the host's
// (Config.Languages), each text missing in one taken from English. An
// error names a code or a text that is not one.
func languages(extra map[string]map[string]string) (map[string]speech, error) {
	out := map[string]speech{}
	for code, sp := range texts {
		out[code] = sp
	}
	for code, given := range extra {
		if !validCode.MatchString(code) {
			return nil, errors.New(ErrConfig, "not a language code (such as de or pt): "+code)
		}
		sp := speech{}
		for m, s := range texts[English] {
			sp[m] = s
		}
		for name, s := range given {
			m := msg(name)
			if _, ok := sp[m]; !ok {
				return nil, errors.New(ErrConfig, "the Bridge has no text "+name+" (see channel.Texts)")
			}
			if strings.Count(s, "{}") != strings.Count(sp[m], "{}") {
				return nil, errors.New(ErrConfig, "text "+name+" in "+code+" must have as many {} as in English")
			}
			sp[m] = s
		}
		out[code] = sp
	}
	return out, nil
}

var validCode = regexp.MustCompile(`^[a-z]{2,3}$`)

// speech is the texts of one language.
type speech map[msg]string

// lang is the language a chat is spoken to in (callable with b.mu held): its user's, as the app
// says it, when the Bridge speaks it; English when it does not; when the
// app says nothing, Config.Language, else English.
func (b *Bridge) lang(chatID string) speech {
	b.langMu.Lock()
	code := b.langs[chatID]
	b.langMu.Unlock()
	if code == "" {
		code = b.cfg.Language
	}
	if l, ok := b.spoken[code]; ok {
		return l
	}
	return b.spoken[English]
}

// hearLanguage notes the language of a chat's sender, as the app says
// it ("it", "it-IT", "pt_BR"): its first part, lower-cased.
func (b *Bridge) hearLanguage(chatID, tag string) {
	code, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tag)), "-")
	code, _, _ = strings.Cut(code, "_")
	if code == "" {
		return
	}
	b.langMu.Lock()
	defer b.langMu.Unlock()
	if len(b.langs) >= maxTracked {
		clear(b.langs) // the default until they write again
	}
	b.langs[chatID] = code
}

// tr is a text in a chat's language, its {} filled with args in order.
func (b *Bridge) tr(chatID string, m msg, args ...string) string {
	return b.lang(chatID).t(m, args...)
}

// t is a text of sp, its {} filled with args in order.
func (sp speech) t(m msg, args ...string) string {
	parts := strings.Split(sp[m], "{}")
	var out strings.Builder
	for i, p := range parts {
		out.WriteString(p)
		if i < len(parts)-1 && i < len(args) { // an arg is never read for {} itself
			out.WriteString(args[i])
		}
	}
	return out.String()
}
