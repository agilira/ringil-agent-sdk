// tool_test.go: create_trigger
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
)

func callCreate(t *testing.T, tool agent.Tool, in string) (string, error) {
	t.Helper()
	reg, err := agent.NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Execute(context.Background(), agent.ToolCall{ID: "c1", Name: "create_trigger", Input: json.RawMessage(in)})
	return res.Content, err
}

func TestCreateTool(t *testing.T) {
	dir := t.TempDir()
	open := func() (*Store, error) { return OpenStore(dir) }
	var added []string
	tool := CreateTool(open, ToolOptions{
		Check:    onlyTools("write_file", "mail_unread"),
		Defaults: func(_ context.Context, tr *Trigger) { tr.Deliver = "telegram-42" },
		Added:    func(tr Trigger) { added = append(added, tr.Name) },
	})
	if !tool.Spec().Annotations.Destructive {
		t.Fatal("not asked")
	}
	if _, err := callCreate(t, tool, `{"name":"morning","kind":"time","when":"0 8 * * 1-5","skill":"brief","allow":["write_file"]}`); err != nil {
		t.Fatal(err)
	}
	store, _ := OpenStore(dir)
	defer func() { _ = store.Close() }()
	list, _ := store.Triggers()
	if len(list) != 1 || list[0].Deliver != "telegram-42" || len(added) != 1 {
		t.Fatalf("%+v %v", list, added)
	}
}

func TestCreateToolRefuses(t *testing.T) {
	dir := t.TempDir()
	tool := CreateTool(func() (*Store, error) { return OpenStore(dir) }, ToolOptions{Check: onlyTools("write_file", "mail_unread")})
	if _, err := callCreate(t, tool, `{"name":"morning","kind":"time","when":"@daily","prompt":"x"}`); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"taken":         `{"name":"morning","kind":"time","when":"@daily","prompt":"x"}`,
		"webhook":       `{"name":"hook","kind":"webhook","prompt":"x"}`,
		"unknown tool":  `{"name":"inbox","kind":"tool","tool":"mail_all","prompt":"x"}`,
		"unknown allow": `{"name":"a","kind":"time","when":"@daily","prompt":"x","allow":["wipe"]}`,
		"invalid":       `{"name":"a","kind":"time","prompt":"x"}`,
	} {
		if _, err := callCreate(t, tool, in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// onlyTools is a host's Check that knows only these tools.
func onlyTools(names ...string) func(context.Context, Trigger) error {
	return func(_ context.Context, tr Trigger) error {
		for _, a := range append([]string{tr.Tool}, tr.Allow...) {
			if a != "" && !slices.Contains(names, a) {
				return errors.New("no such tool " + a)
			}
		}
		return nil
	}
}
