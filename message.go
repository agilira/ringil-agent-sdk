// message.go: conversation messages, tool calls and usage
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"
)

// ErrImage: an image the model APIs would not take.
const ErrImage errors.ErrorCode = "IMAGE"

// Role identifies who produced a [Message].
type Role string

// Roles of a conversation. The system prompt is not a message: it travels in
// [Request.System], so providers that want it separate get it separate and
// host context never leaks into the stored conversation.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	// RoleSystem is an instruction from the host in the middle of the
	// conversation, with the authority of the system prompt: it comes
	// after the turn it concerns, so the system prompt and the history
	// before it stay as they were, and in the provider's cache. Providers
	// send it as their API allows (Claude: a mid-conversation system
	// message; OpenAI-compatible servers, many of which take a system
	// message only first: a marked user message).
	RoleSystem Role = "system"
)

// SystemText returns a mid-conversation system message holding text.
func SystemText(text string) Message {
	return Message{Role: RoleSystem, Parts: []Part{{Type: PartText, Text: text}}}
}

// ContextMemory is the Source of the memories recalled for a turn; the
// host's blocks have "host:" and their name (ContextBlock.Name).
const (
	ContextMemory = "memory"
	contextHost   = "host:"
)

// PartType identifies the kind of content a [Part] carries.
type PartType string

// Part types. Others (reasoning) will be added as new constants and
// fields; Part is a struct so that adding them does not break existing
// code.
const (
	// PartText is plain text.
	PartText PartType = "text"
	// PartImage is an image a user sent: MIME and Data. It is kept whole
	// in the conversation; [CheckImage] says which ones are accepted.
	PartImage PartType = "image"
	// PartContext is text the host added to a user message, not the
	// user's words: the memories recalled for it and the run's context
	// (Config.Context). It goes to the model with the message and stays in
	// the conversation, as mature agents keep their reminders: the system
	// prompt and the history before it never change between turns, so the
	// provider's prompt cache keeps them. [Message.Text] leaves it out;
	// [Message.ModelText] includes it.
	//
	// Source says which: ContextMemory, or "host:" and the name of one of
	// the host's blocks (Config.Context). A turn adds only what is new:
	// memories not recalled earlier in the conversation, and each block
	// when it changed (as Claude Code adds its reminders), so the history
	// does not grow by the same text each turn.
	PartContext PartType = "context"
	// PartReasoning is what a model thought before it answered, as its
	// provider gave it: Text (possibly empty: Claude omits it by default),
	// and Signature, opaque, which the provider checks when the thinking
	// comes back to it; Redacted marks thinking the provider encrypted
	// whole (in Signature). Source names the provider that made it
	// ("anthropic", "openai:<model>"): providers send back only their own.
	// It is part of an assistant message, before its text; [Message.Text]
	// and [Message.ModelText] leave it out.
	PartReasoning PartType = "reasoning"
)

// Part is one piece of message content.
type Part struct {
	Type PartType `json:"type"`
	Text string   `json:"text,omitempty"`
	// MIME and Data are an image's: image/jpeg, image/png, image/gif or
	// image/webp, and its bytes.
	MIME string `json:"mime,omitempty"`
	Data []byte `json:"data,omitempty"`
	// Signature and Redacted are a reasoning part's; Source a reasoning
	// part's or a context part's.
	Signature string `json:"signature,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`
	Source    string `json:"source,omitempty"`
}

// Reasoning is m's reasoning parts made by source, exactly (a provider's
// name, as in PartReasoning: "openai:gpt-5" is not "openai:gpt-5-mini").
func (m Message) Reasoning(source string) []Part {
	var out []Part
	for _, p := range m.Parts {
		if p.Type == PartReasoning && p.Source == source {
			out = append(out, p)
		}
	}
	return out
}

// MaxImageBytes bounds an image: the smallest limit of the main model
// APIs.
const MaxImageBytes = 5 << 20

// imageTypes are the image formats the model APIs take.
var imageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// CheckImage accepts an image the model APIs take: at most MaxImageBytes,
// of a format they read, and of the format its bytes are, whatever its
// label says.
func CheckImage(mime string, data []byte) error {
	switch {
	case len(data) == 0 || len(data) > MaxImageBytes:
		return errors.New(ErrImage, "an image must be 1 byte to 5 MiB")
	case !imageTypes[mime]:
		return errors.New(ErrImage, "images must be JPEG, PNG, GIF or WebP")
	case http.DetectContentType(data) != mime:
		return errors.New(ErrImage, "the image is not the format it says")
	}
	return nil
}

// Image returns an image part, checked by CheckImage; its MIME type is
// taken from its bytes.
func Image(data []byte) (Part, error) {
	mime := http.DetectContentType(data)
	if err := CheckImage(mime, data); err != nil {
		return Part{}, err
	}
	return Part{Type: PartImage, MIME: mime, Data: data}, nil
}

// UserMessage returns a user message holding text and images (parts of
// type PartImage); text may be empty when there are images.
func UserMessage(text string, images ...Part) Message {
	var parts []Part
	if text != "" {
		parts = append(parts, Part{Type: PartText, Text: text})
	}
	return Message{Role: RoleUser, Parts: append(parts, images...)}
}

// Images counts m's images.
func (m Message) Images() int {
	n := 0
	for _, part := range m.Parts {
		if part.Type == PartImage {
			n++
		}
	}
	if m.Result != nil {
		n += len(m.Result.Images)
	}
	return n
}

// ToolCall is a request from the model to run a tool.
//
// Input is always valid JSON. When the model produced arguments that are not
// valid JSON, the provider adapter sets Input to an empty object and BadInput
// to true, so the agent loop can ask the model to try again while the stored
// conversation stays serializable.
type ToolCall struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	BadInput bool            `json:"bad_input,omitempty"`
}

// ToolResult answers one [ToolCall].
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
	// Images are the result's images (parts of type PartImage).
	Images []Part `json:"images,omitempty"`
}

// Message is one turn of a conversation. Assistant messages may carry tool
// calls; tool messages carry exactly one result.
type Message struct {
	Role   Role        `json:"role"`
	Parts  []Part      `json:"parts,omitempty"`
	Calls  []ToolCall  `json:"calls,omitempty"`
	Result *ToolResult `json:"result,omitempty"`
}

// UserText returns a user message holding text.
func UserText(text string) Message {
	return Message{Role: RoleUser, Parts: []Part{{Type: PartText, Text: text}}}
}

// AssistantText returns an assistant message holding text.
func AssistantText(text string) Message {
	return Message{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: text}}}
}

// ToolMessage returns a tool message holding result.
func ToolMessage(result ToolResult) Message {
	return Message{Role: RoleTool, Result: &result}
}

// ModelText is what the model reads of m's text: its context parts, then
// its text parts.
func (m Message) ModelText() string {
	var ctx, text strings.Builder
	for _, part := range m.Parts {
		switch part.Type {
		case PartContext:
			ctx.WriteString(part.Text + "\n\n")
		case PartText:
			text.WriteString(part.Text)
		}
	}
	return ctx.String() + text.String()
}

// Text concatenates the text parts of m.
func (m Message) Text() string {
	var b strings.Builder
	for _, part := range m.Parts {
		if part.Type == PartText {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// Clone returns a deep copy of m, so a caller that keeps a message cannot be
// affected by later changes made by whoever produced it.
func (m Message) Clone() Message {
	m.Parts = append([]Part(nil), m.Parts...)
	for i := range m.Parts {
		if m.Parts[i].Data != nil {
			m.Parts[i].Data = append([]byte(nil), m.Parts[i].Data...)
		}
	}
	if m.Calls != nil {
		calls := make([]ToolCall, len(m.Calls))
		for i, call := range m.Calls {
			call.Input = append(json.RawMessage(nil), call.Input...)
			calls[i] = call
		}
		m.Calls = calls
	}
	if m.Result != nil {
		result := *m.Result
		result.Images = append([]Part(nil), result.Images...)
		for i := range result.Images {
			result.Images[i].Data = append([]byte(nil), result.Images[i].Data...)
		}
		m.Result = &result
	}
	return m
}

// Usage reports the tokens a model call consumed, as the provider stated
// them. Zero means not reported. Input is the whole prompt, the part read
// from the provider's cache (CacheRead) and the part written to it
// (CacheWrite) included: adapters whose API counts them apart add them.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
}

// Add returns the sum of u and other.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		Input:      u.Input + other.Input,
		Output:     u.Output + other.Output,
		CacheRead:  u.CacheRead + other.CacheRead,
		CacheWrite: u.CacheWrite + other.CacheWrite,
	}
}

// StopReason says why a model call or a run ended.
type StopReason string

// Stop reasons.
const (
	StopEnd       StopReason = "end"        // the model finished its answer
	StopToolUse   StopReason = "tool_use"   // the model asked for tools
	StopMaxTokens StopReason = "max_tokens" // the output token limit was reached
	StopFiltered  StopReason = "filtered"   // the provider withheld content
	StopMaxTurns  StopReason = "max_turns"  // the host's turn limit was reached
	StopCancelled StopReason = "cancelled"  // the context was cancelled
	StopLoop      StopReason = "loop"       // the loop detector stopped the run (LoopStop)
	StopDenied    StopReason = "denied"     // the host's approval stopped the run (Decision.Stop)
)

// latestImages is msgs with the images of the keep latest messages that
// have some, and a note in place of the earlier ones (Config.KeepImages);
// keep 0 keeps them all. msgs is not changed.
func latestImages(msgs []Message, keep int) []Message {
	if keep <= 0 {
		return msgs
	}
	last, seen := -1, 0
	for i := len(msgs) - 1; i >= 0 && seen < keep; i-- {
		if msgs[i].Images() > 0 {
			last, seen = i, seen+1
		}
	}
	if seen < keep {
		return msgs
	}
	out := append([]Message(nil), msgs...)
	for i := range last {
		if n := out[i].Images(); n > 0 {
			out[i] = imagesAsNote(out[i], n)
		}
	}
	return out
}

// imagesAsNote is m with its n images replaced by a note at the end.
func imagesAsNote(m Message, n int) Message {
	if m.Result != nil && len(m.Result.Images) > 0 { // a tool's images
		res := *m.Result
		res.Images = nil
		res.Content += "\n[" + strconv.Itoa(len(m.Result.Images)) + " images the tool returned here are no longer shown]"
		m.Result = &res
		return m
	}
	parts := make([]Part, 0, len(m.Parts)+1)
	for _, p := range m.Parts {
		if p.Type != PartImage {
			parts = append(parts, p)
		}
	}
	note := "[an image sent here earlier is no longer shown]"
	if n > 1 {
		note = "[" + strconv.Itoa(n) + " images sent here earlier are no longer shown]"
	}
	m.Parts = append(parts, Part{Type: PartText, Text: "\n" + note})
	return m
}
