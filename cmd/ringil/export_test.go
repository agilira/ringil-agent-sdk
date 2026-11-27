// export_test.go: the state exported, and imported back
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
)

func writeState(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readState(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// An export, a change, an import: the state is the archive's again, and
// a copy of the state before is kept in the data directory.
func TestStateExportImport(t *testing.T) {
	h := newHarness(t, "")
	root := filepath.Join(h.dir, "ringil")
	writeState(t, filepath.Join(root, "sessions", "abc.jsonl"), "hello")
	writeState(t, filepath.Join(root, "ringil.yaml"), "models: [openai:m]\n")
	writeState(t, filepath.Join(root, "triggers", "t.json"), "{}")
	writeState(t, filepath.Join(h.dir, "data", "inbox", "c1", "doc.pdf"), "%PDF")
	writeState(t, filepath.Join(root, "oauth", "srv.json"), "TOKEN")
	writeState(t, filepath.Join(root, "telegram-sessions.json"), `{"chat":"abc"}`)
	archive := filepath.Join(t.TempDir(), "state.tar.gz")
	if code := h.run("state", "export", archive); code != exitOK {
		t.Fatalf("export %d: %s", code, h.err)
	}
	writeState(t, filepath.Join(root, "sessions", "abc.jsonl"), "changed")
	writeState(t, filepath.Join(root, "sessions", "later.jsonl"), "after")
	if code := h.run("state", "import", archive); code != exitOK {
		t.Fatalf("import %d: %s", code, h.err)
	}
	if got := readState(t, filepath.Join(root, "sessions", "abc.jsonl")); got != "hello" {
		t.Fatalf("session %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "sessions", "later.jsonl")); !os.IsNotExist(err) {
		t.Fatal("a session made after the export survived")
	}
	if got := readState(t, filepath.Join(root, "telegram-sessions.json")); got != `{"chat":"abc"}` {
		t.Fatalf("chat records %q", got)
	}
	if got := readState(t, filepath.Join(h.dir, "data", "inbox", "c1", "doc.pdf")); got != "%PDF" {
		t.Fatalf("inbox %q", got)
	}
	copies, _ := filepath.Glob(filepath.Join(h.dir, "data", "backups", "before-import-*.tar.gz"))
	if len(copies) != 1 || !strings.Contains(h.err.String(), "the state before is in") {
		t.Fatalf("copies %v, said %q", copies, h.err)
	}
	checkNoCredentials(t, archive)
}

// checkNoCredentials: sign-ins and the like never leave in an archive.
func checkNoCredentials(t *testing.T, archive string) {
	t.Helper()
	h := newHarness(t, "")
	if code := h.run("state", "verify", archive); code != exitOK {
		t.Fatalf("verify %d: %s", code, h.err)
	}
	b, _ := os.ReadFile(archive)
	if bytes.Contains(b, []byte("TOKEN")) {
		t.Fatal("a sign-in token in the archive")
	}
}

// A running ringil holds the state: an import waits for it to end.
func TestImportRefusedWhileInUse(t *testing.T) {
	h := newHarness(t, "")
	archive := filepath.Join(t.TempDir(), "state.tar.gz")
	if code := h.run("state", "export", archive); code != exitOK {
		t.Fatalf("export %d: %s", code, h.err)
	}
	user := newHarness(t, "")
	user.c.configDir = h.c.configDir
	user.c.dataDir = h.c.dataDir
	loaded(t, user.c, nil, func(*orpheus.Context) error {
		if code := h.run("state", "import", archive); code == exitOK || !strings.Contains(h.err.String(), "running") {
			t.Fatalf("imported under a running ringil: %d %s", code, h.err)
		}
		return nil
	})
	if copies, _ := filepath.Glob(filepath.Join(h.dir, "data", "backups", "*")); len(copies) != 0 {
		t.Fatalf("a copy left for an import that never ran: %v", copies)
	}
}

// From the app: the archive is taken and checked, serve is stopped, and
// once it has ended the import runs and it starts again.
func TestImportFromTheApp(t *testing.T) {
	h := newHarness(t, "")
	writeConfig(t, h.dir, "models: [openai:m]\n")
	session := filepath.Join(h.dir, "ringil", "sessions", "s1.jsonl")
	writeState(t, session, "kept")
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		a, err := agent.New(agent.Config{Provider: fake.New()})
		if err != nil {
			t.Fatal(err)
		}
		srv, err := serve.New(serve.Config{Token: settingsToken, NoStore: true, Profiles: []serve.Profile{{Name: "default", New: serve.Fixed(a)}}})
		if err != nil {
			t.Fatal(err)
		}
		h.c.mountState(ctx, srv)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		call := caller(t, ts.URL)
		code, archive := call("GET", "/v1/state/export", "")
		if code != http.StatusOK || len(archive) < 20 {
			t.Fatalf("export %d", code)
		}
		writeState(t, session, "changed")
		stopped := make(chan struct{})
		h.c.imports.stop = func() { close(stopped) }
		for _, c := range []struct {
			path, body string
			want       int
			ok         bool
		}{
			{"/v1/state/import", "garbage", http.StatusBadRequest, true},
			{"/v1/state/import", archive, http.StatusAccepted, true},
			{"/v1/runs", `{"profile":"default","input":"x"}`, http.StatusCreated, false}, // paused for the import
		} {
			if code, out := call("POST", c.path, c.body); (code == c.want) != c.ok {
				t.Fatalf("%s: %d %s", c.path, code, out)
			}
		}
		<-stopped
		h.c.shutdown() // as serve's end does: the state let go
		checkAppImported(t, h, call, session)
		return nil
	})
}

// caller sends requests with the token to url.
func caller(t *testing.T, url string) func(method, p, body string) (int, string) {
	return func(method, p, body string) (int, string) {
		req, _ := http.NewRequest(method, url+p, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+settingsToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
}

// checkAppImported: the import pending ran once serve let go, and the
// app can read how it went.
func checkAppImported(t *testing.T, h *harness, call func(method, p, body string) (int, string), session string) {
	t.Helper()
	if !h.c.pendingImport() || readState(t, session) != "kept" {
		t.Fatalf("not imported: %s", h.err)
	}
	if code, out := call("GET", "/v1/state/import", ""); code != http.StatusOK || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("last import %d %s", code, out)
	}
}
