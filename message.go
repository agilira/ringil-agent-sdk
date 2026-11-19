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
)

// Part is one piece of message content.
type Part struct {
	Type PartType `json:"type"`
	Text string   `json:"text,omitempty"`
	// MIME and Data are an image's: image/jpeg, image/png, image/gif or
	// image/webp, and its bytes.
	MIME string `json:"mime,omitempty"`
	Data []byte `json:"data,omitempty"`
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
		m.Result = &result
	}
	return m
}

// Usage reports the tokens a model call consumed, as the provider stated
// them. Zero means not reported.
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
)

// latestImagesOnly is msgs with the images of the last message that has
// some, and a note in place of the earlier ones: a conversation does not
// send every image it ever had on every turn (some servers take one image
// per request; all of them charge for each). msgs is not changed.
func latestImagesOnly(msgs []Message) []Message {
	last := -1
	for i := len(msgs) - 1; i >= 0 && last < 0; i-- {
		if msgs[i].Images() > 0 {
			last = i
		}
	}
	if last < 0 {
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
