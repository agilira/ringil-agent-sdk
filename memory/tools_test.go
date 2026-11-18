// tools_test.go: tests for the memory tools
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package memory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/memory/local"
)

func call(t *testing.T, r agent.Registry, name string, args map[string]any) (string, error) {
	t.Helper()
	in, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Execute(context.Background(), agent.ToolCall{ID: "1", Name: name, Input: in})
	return res.Content, err
}

func must(t *testing.T, r agent.Registry, name string, args map[string]any) string {
	t.Helper()
	out, err := call(t, r, name, args)
	if err != nil {
		t.Fatalf("%s: %v (%s)", name, err, out)
	}
	return out
}

func registry(t *testing.T) agent.Registry {
	t.Helper()
	m, err := local.New(filepath.Join(t.TempDir(), "m"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := agent.NewRegistry(Tools(m)...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRememberRecallForget(t *testing.T) {
	r := registry(t)
	out := must(t, r, "remember", map[string]any{"content": "Il cliente preferisce le call al mattino"})
	id := regexp.MustCompile(`Remembered as (\w+)\.`).FindStringSubmatch(out)
	if id == nil {
		t.Fatalf("remember %q", out)
	}
	if out := must(t, r, "recall", map[string]any{"query": "quando fare le call col cliente"}); !strings.Contains(out, "["+id[1]+"] Il cliente preferisce") {
		t.Fatalf("recall %q", out)
	}
	if out := must(t, r, "forget", map[string]any{"id": id[1]}); out != "Forgotten." {
		t.Fatalf("forget %q", out)
	}
	if out := must(t, r, "recall", map[string]any{}); out != "Nothing remembered matches." {
		t.Fatalf("recall after forget %q", out)
	}
}

func TestToolErrors(t *testing.T) {
	r := registry(t)
	if _, err := call(t, r, "forget", map[string]any{"id": "missing"}); !errors.HasCode(err, agent.ErrMemoryNotFound) {
		t.Fatalf("forget missing %v", err)
	}
	if _, err := call(t, r, "remember", map[string]any{"content": " "}); !errors.HasCode(err, agent.ErrMemoryInput) {
		t.Fatalf("empty remember %v", err)
	}
}

func TestAnnotations(t *testing.T) {
	for _, tool := range Tools(nil) {
		spec := tool.Spec()
		if spec.Name == "recall" && !spec.Annotations.ReadOnly || spec.Name == "forget" && !spec.Annotations.Destructive {
			t.Errorf("%s annotations %+v", spec.Name, spec.Annotations)
		}
	}
}
