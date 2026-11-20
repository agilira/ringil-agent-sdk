// security_test.go: fuzz and privilege-boundary tests for triggers and store hardening
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"
)

func FuzzParseWhen(f *testing.F) {
	seeds := []string{
		"* * * * *",
		"0 8 * * 1-5",
		"0 0 29 2 *",
		"0 0 30 2 *",
		"0 0 */2 * 1",
		"59 23 31 12 0-7",
		"@daily",
		"@hourly",
		"@yearly",
		"@every 1m",
		"@every 30s",
		"@every 0s",
		"@every -5m",
		"@every 90s",
		"5-1 * * * *",
		"*/0 * * * *",
		"0 0 * * mon-sun",
		"0 0 1 jan-dec *",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	locs := []*time.Location{time.UTC}
	if ny, err := time.LoadLocation("America/New_York"); err == nil {
		locs = append(locs, ny)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		if len(spec) > 512 {
			return
		}
		for _, loc := range locs {
			checkParsedWhen(t, spec, loc)
		}
	})
}

func checkParsedWhen(t *testing.T, spec string, loc *time.Location) {
	t.Helper()
	w, err := Parse(spec, loc)
	if err != nil {
		return
	}
	ref := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)
	next := w.Next(ref)
	if !next.IsZero() && !next.After(ref) {
		t.Fatalf("Parse(%q).Next(%v) = %v is not strictly after ref", spec, ref, next)
	}
	if ev, ok := w.(every); ok {
		if ev.d < MinInterval || ev.d%time.Minute != 0 {
			t.Fatalf("Parse(%q) produced invalid interval %v", spec, ev.d)
		}
	}
}

func FuzzTriggerCheck(f *testing.F) {
	seeds := []string{
		`{"name":"morning","kind":"time","when":"0 8 * * 1-5","prompt":"check"}`,
		`{"name":"hook","kind":"webhook","secret":"01234567890123456789012345678901","skill":"triage"}`,
		`{"name":"msg","kind":"message","chat":"room1","match":"deploy","prompt":"go"}`,
		`{"name":"watch","kind":"file","path":"/tmp/x","prompt":"read"}`,
		`{"name":"poll","kind":"tool","tool":"mail","args":{"q":"a"},"prompt":"go"}`,
		`{"name":"../bad","kind":"time","when":"@daily","prompt":"x"}`,
		`{"name":"x","kind":"time","when":"@daily","prompt":"x","secret":"leak"}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 8192 {
			return
		}
		var tr Trigger
		if json.Unmarshal(raw, &tr) != nil {
			return
		}
		_, err := tr.Check()
		_ = tr.Working(now)
		_ = tr.Ended(now)
		_ = Describe(tr)
		if err != nil {
			return
		}
		assertCheckedTrigger(t, tr)
	})
}

func assertCheckedTrigger(t *testing.T, tr Trigger) {
	t.Helper()
	if !validName.MatchString(tr.Name) {
		t.Fatalf("invalid name passed Check: %q", tr.Name)
	}
	if len(tr.Allow) > maxAllow {
		t.Fatalf("too many allow tools passed Check: %d", len(tr.Allow))
	}
	if len(tr.Prompt) > MaxInput {
		t.Fatalf("oversized prompt passed Check: %d", len(tr.Prompt))
	}
	if tr.kind() == Webhook && len(tr.Secret) < minSecret {
		t.Fatalf("short webhook secret passed Check: %d", len(tr.Secret))
	}
}

func TestSecurityCreateTriggerPrivilegeBoundaries(t *testing.T) {
	dir := t.TempDir()
	tool := CreateTool(func() (*Store, error) { return OpenStore(dir) }, ToolOptions{})
	badCalls := map[string]string{
		"webhook":        `{"name":"hook","kind":"webhook","prompt":"hi"}`,
		"message":        `{"name":"msg","kind":"message","prompt":"hi"}`,
		"allowCreate":    `{"name":"esc1","kind":"time","when":"@daily","prompt":"hi","allow":["create_trigger"]}`,
		"allowDelete":    `{"name":"esc2","kind":"time","when":"@daily","prompt":"hi","allow":["delete_trigger"]}`,
		"shadowName":     `{"name":"a","Name":"b","kind":"time","when":"@daily","prompt":"hi"}`,
		"extraSecret":    `{"name":"a","kind":"time","when":"@daily","prompt":"hi","secret":"01234567890123456789012345678901"}`,
		"extraDeliver":   `{"name":"a","kind":"time","when":"@daily","prompt":"hi","deliver":"other-chat"}`,
		"extraProfile":   `{"name":"a","kind":"time","when":"@daily","prompt":"hi","profile":"admin"}`,
		"extraChat":      `{"name":"a","kind":"time","when":"@daily","prompt":"hi","chat":"room"}`,
		"extraPaused":    `{"name":"a","kind":"time","when":"@daily","prompt":"hi","paused":true}`,
		"untilPast":      `{"name":"a","kind":"time","when":"@daily","prompt":"hi","until":"2020-01-01"}`,
		"traversalName":  `{"name":"../evil","kind":"time","when":"@daily","prompt":"hi"}`,
		"slashName":      `{"name":"a/b","kind":"time","when":"@daily","prompt":"hi"}`,
		"emptyName":      `{"name":"","kind":"time","when":"@daily","prompt":"hi"}`,
		"longName":       `{"name":"` + strings.Repeat("a", 49) + `","kind":"time","when":"@daily","prompt":"hi"}`,
		"badSkill":       `{"name":"a","kind":"time","when":"@daily","skill":"Bad_Skill"}`,
		"shortTimeout":   `{"name":"a","kind":"time","when":"@daily","prompt":"hi","timeout":"30s"}`,
		"longTimeout":    `{"name":"a","kind":"time","when":"@daily","prompt":"hi","timeout":"3h"}`,
		"oversizePrompt": `{"name":"a","kind":"time","when":"@daily","prompt":"` + strings.Repeat("x", MaxInput+1) + `"}`,
	}
	for name, in := range badCalls {
		if _, err := callCreate(t, tool, in); err == nil {
			t.Errorf("%s: accepted by create_trigger", name)
		}
	}
	tooManyAllow, _ := json.Marshal(map[string]any{
		"name": "many", "kind": "time", "when": "@daily", "prompt": "hi", "allow": make([]string, 65),
	})
	if _, err := callCreate(t, tool, string(tooManyAllow)); err == nil {
		t.Error("65 allow entries accepted by create_trigger")
	}
}

func TestSecurityStoreHardening(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, triggersFile), []byte(`{"triggers":[],"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.Triggers(); !errors.HasCode(err, ErrStore) {
		t.Fatalf("unknown field accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, triggersFile), []byte(`{corrupt`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Triggers(); !errors.HasCode(err, ErrStore) {
		t.Fatalf("corrupt JSON accepted: %v", err)
	}
	checkPublicDirRefused(t)
}

func checkPublicDirRefused(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	pubDir := filepath.Join(t.TempDir(), "pub")
	if err := os.Mkdir(pubDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pubDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(pubDir); err == nil {
		t.Fatal("OpenStore accepted 0777 directory")
	}
}
