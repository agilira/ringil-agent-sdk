// structured.go: answers in JSON a schema describes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/schema"
)

// ErrFormat: the model's answer is not the JSON its Format asked for.
const ErrFormat errors.ErrorCode = "FORMAT_MISMATCH"

// Format asks the model for an answer in JSON of a type: providers that
// constrain output to a schema are given it (in the form each takes),
// and Parse checks the answer against the type whatever the provider did.
// Make it with FormatFor.
type Format struct {
	// Name names the format to providers that want one.
	Name string
	// Schema is the type's JSON Schema in the form structured output takes:
	// closed objects, no bounds (Parse checks those).
	Schema json.RawMessage
	// Strict: every field is required, as providers' strict modes want.
	Strict bool

	node *schema.Node
	typ  reflect.Type
}

var formatName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// FormatFor is the Format of T, a struct: its fields as tools' inputs
// declare them (json names, `desc`, `min`, `max`, `enum` tags). A map has no
// structured-output form: use a struct or a list of them.
func FormatFor[T any](name string) (*Format, error) {
	if !formatName.MatchString(name) {
		return nil, errors.New(ErrFormat, "a format's name is 1-64 letters, digits, _ or -")
	}
	t := reflect.TypeFor[T]()
	if t.Kind() != reflect.Struct || t == reflect.TypeFor[time.Time]() {
		return nil, errors.New(ErrFormat, "format "+name+": the answer is an object: give a struct")
	}
	node, err := schema.For(t)
	if err != nil {
		return nil, errors.Wrap(err, ErrFormat, "format "+name+": "+err.Error())
	}
	doc, strict, err := node.Portable()
	if err != nil {
		return nil, errors.Wrap(err, ErrFormat, "format "+name+": "+err.Error())
	}
	return &Format{Name: name, Schema: doc, Strict: strict, node: node, typ: t}, nil
}

// Parse decodes text, the model's answer under f, as T, after checking it
// against T's schema: bounds and types included, whatever the provider
// enforced. A fenced block (```json) around it is taken off. Errors name
// the fields that are wrong, never the answer.
func Parse[T any](f *Format, text string) (T, error) {
	var out T
	if f == nil || f.node == nil {
		return out, errors.New(ErrFormat, "Parse takes a Format made by FormatFor")
	}
	if reflect.TypeFor[T]() != f.typ {
		return out, errors.New(ErrFormat, "Parse's type does not match format "+f.Name)
	}
	body := unfence(text)
	if !strings.HasPrefix(body, "{") {
		// Empty, null or prose: a truncated or refused answer, never an
		// empty object (as a tool's missing arguments would be).
		return out, errors.New(ErrFormat, "the answer is not the "+f.Name+" asked: it is no JSON object")
	}
	canonical, ignored, problems := f.node.Validate(json.RawMessage(body))
	if len(problems) > 0 {
		return out, formatProblems(f.Name, problems)
	}
	if ignored > 0 {
		// A field it does not define, a misspelt one: its value would be lost.
		return out, errors.New(ErrFormat, "the answer is not the "+f.Name+" asked: it has fields "+f.Name+" does not define")
	}
	if err := json.Unmarshal(canonical, &out); err != nil {
		return out, errors.New(ErrFormat, "the answer does not decode as "+f.Name)
	}
	return out, nil
}

func formatProblems(name string, problems []schema.Problem) error {
	parts := make([]string, len(problems))
	for i, p := range problems {
		path := p.Path
		if path == "arguments" {
			path = "answer"
		}
		parts[i] = path + ": " + p.Message
	}
	return errors.New(ErrFormat, "the answer is not the "+name+" asked: "+strings.Join(parts, "; "))
}

// unfence takes off a ```json fence a model may put around its answer.
func unfence(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	t = strings.TrimPrefix(t, "```")
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:] // the fence's language word
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), "```"))
}
