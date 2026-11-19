// files.go: what is sent with a message, as the agent takes it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Bounds of the files fetched: audio as the transcription APIs take it,
// documents as the chat apps send them.
const (
	MaxAudioBytes    = 25 << 20
	MaxDocumentBytes = 20 << 20
	// maxFiles bounds the files of one message.
	maxFiles = 10
)

// ErrFile: a file sent in a chat could not be taken.
const ErrFile errors.ErrorCode = "CHANNEL_FILE"

// message builds the user message: the text, the voice messages heard,
// the images, and where the documents were saved. problem, when not
// empty, is why it could not be built, for the chat.
func (b *Bridge) message(ctx context.Context, chatID, text string, files []File) (agent.Message, string) {
	if len(files) > maxFiles {
		return agent.Message{}, b.tr(chatID, mMaxFiles)
	}
	var images []agent.Part
	var notes []string
	var saved []string
	for _, f := range files {
		image, note, path, problem := b.take(ctx, chatID, f)
		if path != "" {
			saved = append(saved, path)
		}
		if problem == "" && ctx.Err() != nil {
			problem = b.tr(chatID, mStopped)
		}
		if problem != "" {
			removeAll(saved) // the agent will never hear of them
			return agent.Message{}, problem
		}
		if image != nil {
			images = append(images, *image)
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	return agent.UserMessage(strings.TrimSpace(strings.Join(append([]string{text}, notes...), "\n\n")), images...), ""
}

// take turns one file into an image or a note for the agent; path is
// where a document was saved.
func (b *Bridge) take(ctx context.Context, chatID string, f File) (image *agent.Part, note, path, problem string) {
	switch {
	case f.Voice || (strings.HasPrefix(f.MIME, "audio/") && b.cfg.Transcriber != nil):
		note, problem = b.hear(ctx, chatID, f)
	case modelImage(f.MIME):
		image, problem = b.see(ctx, chatID, f)
	default: // other images and audio are documents
		note, path, problem = b.save(ctx, chatID, f)
	}
	return image, note, path, problem
}

// modelImage: a type the model APIs read (the bytes are checked later).
func modelImage(mime string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

func removeAll(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// hear transcribes a voice message or an audio file.
func (b *Bridge) hear(ctx context.Context, chatID string, f File) (string, string) {
	if b.cfg.Transcriber == nil {
		return "", b.tr(chatID, mNoVoiceIn)
	}
	data, problem := b.fetch(ctx, chatID, f, MaxAudioBytes)
	if problem != "" {
		return "", problem
	}
	text, err := b.cfg.Transcriber.Transcribe(ctx, data, f.MIME)
	if err != nil && ctx.Err() != nil {
		return "", b.tr(chatID, mStopped)
	}
	if err != nil {
		b.report(Event{Kind: FileFailed, Chat: chatID, Err: err})
		return "", b.tr(chatID, mVoiceUnheard)
	}
	if f.Voice {
		return "[voice message, transcribed] " + text, ""
	}
	return "[audio file " + b.displayName(chatID, f) + ", transcribed] " + text, ""
}

// see takes an image, checked on its bytes.
func (b *Bridge) see(ctx context.Context, chatID string, f File) (*agent.Part, string) {
	data, problem := b.fetch(ctx, chatID, f, agent.MaxImageBytes)
	if problem != "" {
		return nil, problem
	}
	p, err := agent.Image(data)
	if err != nil {
		return nil, b.tr(chatID, mImages)
	}
	return &p, ""
}

// save keeps a document in the chat's inbox and tells the agent where.
func (b *Bridge) save(ctx context.Context, chatID string, f File) (note, path, problem string) {
	if b.cfg.Inbox == "" {
		return "", "", b.tr(chatID, mNoDocuments)
	}
	data, problem := b.fetch(ctx, chatID, f, MaxDocumentBytes)
	if problem != "" {
		return "", "", problem
	}
	b.mu.Lock()
	conv := b.chats[chatID].conv
	b.mu.Unlock()
	path, err := b.store(filepath.Join(b.cfg.Inbox, conv), f, data)
	if err != nil {
		b.report(Event{Kind: FileFailed, Chat: chatID, Err: err})
		return "", "", b.tr(chatID, mNotKept, b.displayName(chatID, f))
	}
	return "[the user sent a file, saved as " + path + "]", path, ""
}

// InboxOf is where the documents of a chat's conversation are kept: a
// directory of Config.Inbox named as the conversation, so that a host
// grants each conversation's runs its own documents only. Empty without
// an inbox.
func (b *Bridge) InboxOf(chatID string) string {
	if b.cfg.Inbox == "" {
		return ""
	}
	return filepath.Join(b.cfg.Inbox, b.current(chatID))
}

// fetch downloads a file within max bytes.
func (b *Bridge) fetch(ctx context.Context, chatID string, f File, max int64) ([]byte, string) {
	if f.Size > max {
		return nil, b.tr(chatID, mTooLarge, b.displayName(chatID, f), mib(max))
	}
	if f.Fetch == nil {
		return nil, b.tr(chatID, mNotFetched, b.displayName(chatID, f))
	}
	data, err := f.Fetch(ctx, max)
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, b.tr(chatID, mStopped)
	case err != nil:
		b.report(Event{Kind: FileFailed, Chat: chatID, Err: err})
		return nil, b.tr(chatID, mNotFetched, b.displayName(chatID, f))
	case int64(len(data)) > max:
		return nil, b.tr(chatID, mTooLarge, b.displayName(chatID, f), mib(max))
	}
	return data, ""
}

// store writes a document in the inbox under a new name: the date, a
// random part and the sender's name made safe. Never overwrites.
func (b *Bridge) store(dir string, f File, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errors.Wrap(err, ErrFile, "cannot create the inbox")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", errors.Wrap(err, ErrFile, "cannot open the inbox")
	}
	defer func() { _ = root.Close() }()
	name := time.Now().Format("20060102-150405") + "-" + strings.ToLower(rand.Text()[:6]) + "-" + safeName(f.Name)
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", errors.Wrap(err, ErrFile, "cannot create the file")
	}
	_, err = file.Write(data)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = root.Remove(name)
		return "", errors.Wrap(err, ErrFile, "cannot write the file")
	}
	return filepath.Join(dir, name), nil
}

// safeName keeps letters, digits, '.', '-' and '_' of a sender's file
// name, without leading dots, at most 80 characters; "file" when nothing
// is left.
func safeName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '.', r == '-', r == '_':
			return r
		case r == ' ':
			return '_'
		}
		return -1
	}, filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	name = strings.TrimLeft(name, ".")
	if r := []rune(name); len(r) > 80 {
		name = string(r[len(r)-80:]) // the end keeps the extension
	}
	if name == "" {
		return "file"
	}
	return name
}

// displayName is a file's name for the chat.
func (b *Bridge) displayName(chatID string, f File) string {
	if n := visible(strings.TrimSpace(f.Name)); n != "" {
		return "“" + n + "”"
	}
	return b.tr(chatID, mTheFile)
}

func mib(n int64) string {
	return strconv.FormatInt(n>>20, 10)
}
