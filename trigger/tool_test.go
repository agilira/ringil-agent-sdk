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
	"strings"
	"testing"
	"time"

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

func TestListAndDelete(t *testing.T) {
	dir := t.TempDir()
	open := func() (*Store, error) { return OpenStore(dir) }
	list, del := ListTool(open), DeleteTool(open, nil)
	if !list.Spec().Annotations.ReadOnly || !del.Spec().Annotations.Destructive || !strings.Contains(listOne(t, dir), "No automations") {
		t.Fatal("annotations")
	}
	store, _ := OpenStore(dir)
	if err := store.Put(Trigger{Name: "dec", When: "@daily", Prompt: "send the coupon", From: "2026-12-01", Until: "2026-12-31", Allow: []string{"send_mail"}}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	got, _ := run(list, `{}`)
	for _, want := range []string{"dec: on @daily, from 2026-12-01, until 2026-12-31", "does: send the coupon", "may use unasked: send_mail"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in %q", want, got)
		}
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(dir)
	if err := store.Put(Trigger{Name: "dec", When: "@daily", Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	del := DeleteTool(func() (*Store, error) { return OpenStore(dir) }, nil)
	if _, err := run(del, `{"name":"dec"}`); err != nil {
		t.Fatal(err)
	}
	if got := listOne(t, dir); got != "No automations." {
		t.Fatalf("%q", got)
	}
	if _, err := run(del, `{"name":"dec"}`); err == nil {
		t.Fatal("deleted twice")
	}
}

func run(tool agent.Tool, in string) (string, error) {
	reg, _ := agent.NewRegistry(tool)
	res, err := reg.Execute(context.Background(), agent.ToolCall{ID: "1", Name: tool.Spec().Name, Input: json.RawMessage(in)})
	return res.Content, err
}

func listOne(t *testing.T, dir string) string {
	t.Helper()
	got, err := run(ListTool(func() (*Store, error) { return OpenStore(dir) }), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAPastPeriodIsRefused(t *testing.T) {
	dir := t.TempDir()
	tool := CreateTool(func() (*Store, error) { return OpenStore(dir) }, ToolOptions{})
	if _, err := callCreate(t, tool, `{"name":"old","kind":"time","when":"@daily","prompt":"x","until":"2020-01-01"}`); err == nil {
		t.Fatal("made a trigger whose period is over")
	}
}

func TestRemoveIfJudgesTheStoredVersion(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Put(Trigger{Name: "dec", When: "@daily", Prompt: "x", Until: "2027-12-31"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local)
	if err := store.RemoveIf("dec", func(t Trigger) bool { return t.Ended(now) }); err != nil {
		t.Fatal(err)
	}
	if list, _ := store.Triggers(); len(list) != 1 {
		t.Fatal("removed a trigger extended meanwhile")
	}
}
