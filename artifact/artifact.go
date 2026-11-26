// artifact.go: durable results agents make, version by version
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package artifact keeps what agents make for the user to see and keep —
// a document, a page, a diagram, a component, a piece of code, a file —
// apart from the conversation, as an app shows it beside the chat. An
// artifact has a name within a scope (a conversation, chosen by the host)
// and versions: creating it is version 1, each edit or rewrite adds one,
// and none is ever overwritten. Every version is stored by the SHA-256 of
// its bytes, checked on every read.
//
// Its kind says how an app shows it: markdown, html (a page, shown in a
// sandboxed frame), svg, mermaid, react (a component), code (with its
// language), text, or file (bytes of a media type, such as a spreadsheet
// the agent wrote).
//
// The model works on a store through Tools: it creates an artifact, edits
// it by replacing exact passages (as with a file), rewrites it whole,
// reads and lists them. The host gives the scope of a run with WithScope
// and learns of each new version with WithPublished: a server streams it
// to the app as it happens. Contents are data, never instructions.
package artifact

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

// Error codes.
const (
	ErrInput    errors.ErrorCode = "ARTIFACT_INPUT"
	ErrNotFound errors.ErrorCode = "ARTIFACT_NOT_FOUND"
	ErrExists   errors.ErrorCode = "ARTIFACT_EXISTS"
	ErrConflict errors.ErrorCode = "ARTIFACT_CONFLICT"
	ErrIO       errors.ErrorCode = "ARTIFACT_IO"
	ErrDamaged  errors.ErrorCode = "ARTIFACT_DAMAGED"
)

// Kinds of artifact: how an app shows one.
const (
	Markdown = "markdown"
	HTML     = "html"
	SVG      = "svg"
	Mermaid  = "mermaid"
	React    = "react"
	Code     = "code"
	Text     = "text"
	File     = "file"
)

// kindTypes are the media types of the kinds written as text; a file has
// its own.
var kindTypes = map[string]string{
	Markdown: "text/markdown",
	HTML:     "text/html",
	SVG:      "image/svg+xml",
	Mermaid:  "text/vnd.mermaid",
	React:    "text/jsx",
	Code:     "text/plain",
	Text:     "text/plain",
}

// MediaType is the media type of kind; "" for a file, whose type is its
// own, or a kind that is none of these.
func MediaType(kind string) string { return kindTypes[kind] }

// Textual says whether kind is written as text (all but file).
func Textual(kind string) bool { return kindTypes[kind] != "" }

// Meta is what describes an artifact beside its content.
type Meta struct {
	// Title is what an app shows; default the name.
	Title string
	// Kind is one of the kinds above; default text.
	Kind string
	// Language is a code artifact's language, such as go or python.
	Language string
	// MediaType is a file's media type; for the other kinds it is the
	// kind's.
	MediaType string
	// By names who made this version: a run, a worker, the user.
	By string
	// Base is the version this one follows, checked under the store's
	// lock: a change made on a version that is no longer the latest is
	// refused (ErrConflict) instead of silently undoing the one between.
	// 0 checks nothing; NoBase means the name must not exist yet.
	Base int
}

// NoBase is Meta.Base for a new artifact.
const NoBase = -1

// Info describes one version of an artifact.
type Info struct {
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	Language  string    `json:"language,omitempty"`
	MediaType string    `json:"media_type"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	Created   time.Time `json:"created"`
	By        string    `json:"by,omitempty"`
}

// Store keeps artifacts by scope.
type Store interface {
	// Put adds a version of name to scope and returns its description.
	Put(ctx context.Context, scope, name string, meta Meta, data []byte) (Info, error)
	// Get returns a version of name; version 0 is the latest.
	Get(ctx context.Context, scope, name string, version int) (Info, []byte, error)
	// List returns the latest version of every artifact of scope, by name.
	List(ctx context.Context, scope string) ([]Info, error)
	// Versions returns every version of name kept, oldest first.
	Versions(ctx context.Context, scope, name string) ([]Info, error)
	// Delete removes every version of name.
	Delete(ctx context.Context, scope, name string) error
	// DeleteScope removes every artifact of scope (a conversation gone).
	DeleteScope(ctx context.Context, scope string) error
}

var (
	validName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	validScope = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,99}$`)
	validMedia = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9.+-]+$`)
	validLang  = regexp.MustCompile(`^[a-z0-9][a-z0-9+#._-]{0,31}$`)
)

// ValidScope says whether scope can name a scope: a conversation's ID can.
func ValidScope(scope string) bool { return validScope.MatchString(scope) }

func check(scope, name string) error {
	switch {
	case !validScope.MatchString(scope):
		return errors.New(ErrInput, "scopes are 1-100 letters, digits, . _ -, not starting with a dot")
	case !validName.MatchString(name):
		return errors.New(ErrInput, "artifact names are 1-100 letters, digits, . _ -, starting with a letter or digit").WithUserMessage("Artifact names are 1-100 letters, digits, . _ -, such as report.md or chart.")
	}
	return nil
}

// maxTitle bounds a title, in bytes.
const maxTitle = 200

// normalize is meta as stored: a known kind, its media type, a title.
func normalize(name string, meta Meta) (Meta, error) {
	if meta.Kind == "" {
		meta.Kind = Text
	}
	switch {
	case meta.Kind == File:
		if !validMedia.MatchString(meta.MediaType) {
			meta.MediaType = "application/octet-stream"
		}
	case Textual(meta.Kind):
		meta.MediaType = MediaType(meta.Kind)
	default:
		return Meta{}, errors.New(ErrInput, "unknown kind "+meta.Kind).WithUserMessage("The kind is one of markdown, html, svg, mermaid, react, code, text, file.")
	}
	if meta.Kind != Code {
		meta.Language = ""
	} else if meta.Language != "" && !validLang.MatchString(meta.Language) {
		return Meta{}, errors.New(ErrInput, "bad language").WithUserMessage("The language is a short lowercase name, such as go, python or typescript.")
	}
	meta.Title = cleanTitle(meta.Title)
	if meta.Title == "" {
		meta.Title = name
	}
	return meta, nil
}

// cleanTitle is one line of at most maxTitle bytes, without control
// characters.
func cleanTitle(t string) string {
	t = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(t, ""))
	t = strings.TrimSpace(t)
	if len(t) <= maxTitle {
		return t
	}
	cut := maxTitle
	for cut > 0 && !utf8.RuneStart(t[cut]) {
		cut--
	}
	return strings.TrimSpace(t[:cut])
}

type (
	scopeKey     struct{}
	publishedKey struct{}
)

// WithScope makes scope the only scope of the tools' calls under ctx,
// over Access.Scope and Access.Read: a server gives each run its
// conversation's, workers included. "" means none: a run without a
// conversation has no artifacts.
func WithScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// ScopeOf is the scope WithScope gave ctx, and whether it gave one.
func ScopeOf(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(scopeKey{}).(string)
	return s, ok
}

// WithPublished has fn called with each version the tools under ctx add:
// a server streams it to the app. fn must not block.
func WithPublished(ctx context.Context, fn func(Info)) context.Context {
	return context.WithValue(ctx, publishedKey{}, fn)
}

func published(ctx context.Context, info Info) {
	if fn, ok := ctx.Value(publishedKey{}).(func(Info)); ok && fn != nil {
		fn(info)
	}
}
