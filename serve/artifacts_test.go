// artifacts_test.go: what agents make, as the app sees it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/artifact"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/session/file"
)

// artifactAPI is a server whose agent makes a page in its first turn.
func artifactAPI(t *testing.T) (*api, *artifact.Local) {
	t.Helper()
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	arts, err := artifact.NewLocal(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	tools, err := agent.NewRegistry(artifact.Tools(arts, artifact.Access{Write: true})...)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"name": "page", "title": "Landing", "kind": "html", "content": "<script>alert(1)</script><h1>Hi</h1>"})
	model := fake.New(fake.Step{Calls: []agent.ToolCall{{Name: "create_artifact", Input: input}}}, fake.Reply("made it"))
	cfg := agent.Config{Provider: model, Tools: tools, Approve: func(context.Context, agent.ToolCall, agent.ToolSpec) (agent.Decision, error) {
		return agent.Allow(), nil
	}}
	return newAPI(t, Config{Profiles: []Profile{profile(t, "default", cfg)}, Store: store, Artifacts: arts}), arts
}

// A run's artifact lands in its conversation, is an event of the run,
// and reads back as text, never as a page of this origin.
func TestArtifactsOfAConversation(t *testing.T) {
	a, arts := artifactAPI(t)
	body, _ := json.Marshal(map[string]string{"profile": "default", "conversation": "chat1", "input": "make a page"})
	run := a.start(string(body))
	a.until(run.ID, func(s Snapshot) bool { return s.State != Running && s.State != Waiting })
	if kinds, _ := a.events(run.ID); !slices.Contains(kinds, "artifact") {
		t.Fatalf("no artifact event in %v", kinds)
	}
	checkPageRead(t, a)
	var versions ArtifactVersions
	a.json("GET", "/v1/conversations/chat1/artifacts/page", http.StatusOK, &versions)
	if versions.Latest.Version != 1 || len(versions.Versions) != 1 {
		t.Fatalf("%+v", versions)
	}
	a.json("DELETE", "/v1/conversations/chat1", http.StatusNoContent, nil)
	if left, _ := arts.List(t.Context(), "chat1"); len(left) != 0 {
		t.Fatal("the conversation's artifacts stayed")
	}
}

// A file the agent saved is a download; a raster image is shown as
// itself; a version that does not exist is 404.
func TestArtifactDownloads(t *testing.T) {
	a, arts := artifactAPI(t)
	a.ran("chat2", "make a page")
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	for _, c := range []struct {
		name, ctype, disposition string
		meta                     artifact.Meta
	}{
		{"chart.png", "image/png", "", artifact.Meta{Kind: artifact.File, MediaType: "image/png"}},
		{"sheet.xlsx", "application/octet-stream", `attachment; filename="sheet.xlsx"`, artifact.Meta{Kind: artifact.File, MediaType: "application/vnd.ms-excel"}},
		{"page.svg", "text/plain; charset=utf-8", "", artifact.Meta{Kind: artifact.SVG}},
	} {
		if _, err := arts.Put(t.Context(), "chat2", c.name, c.meta, png); err != nil {
			t.Fatal(err)
		}
		resp := a.raw("GET", "/v1/conversations/chat2/artifacts/"+c.name+"/content", "Authorization", "Bearer "+token)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != c.ctype || resp.Header.Get("Content-Disposition") != c.disposition {
			t.Errorf("%s: %d %q %q", c.name, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"))
		}
	}
	checkArtifactRefusals(t, a)
}

func checkArtifactRefusals(t *testing.T, a *api) {
	t.Helper()
	for path, code := range map[string]int{
		"/v1/conversations/chat2/artifacts/chart.png/content?version=9": http.StatusNotFound,
		"/v1/conversations/chat2/artifacts/chart.png/content?version=x": http.StatusBadRequest,
		"/v1/conversations/chat2/artifacts/nope":                        http.StatusNotFound,
		"/v1/conversations/bad.id/artifacts":                            http.StatusBadRequest,
		"/v1/conversations/nobody/artifacts":                            http.StatusNotFound,
	} {
		if got, out := a.do("GET", path, ""); got != code {
			t.Errorf("%s: %d %s", path, got, out)
		}
	}
	if code, _ := a.do("GET", "/v1/conversations/chat2/artifacts", "", "Authorization", ""); code != http.StatusUnauthorized {
		t.Errorf("without the token: %d", code)
	}
}

// checkPageRead: the page is listed, and reads back as text.
func checkPageRead(t *testing.T, a *api) {
	t.Helper()
	var list []artifact.Info
	a.json("GET", "/v1/conversations/chat1/artifacts", http.StatusOK, &list)
	if len(list) != 1 || list[0].Name != "page" || list[0].Kind != artifact.HTML || list[0].Title != "Landing" {
		t.Fatalf("%+v", list)
	}
	resp := a.raw("GET", "/v1/conversations/chat1/artifacts/page/content", "Authorization", "Bearer "+token)
	got := body(resp)
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" || resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") || !strings.Contains(got, "<h1>Hi</h1>") {
		t.Fatalf("%s cc=%q %q %q", ct, resp.Header.Get("Cache-Control"), resp.Header.Get("Content-Security-Policy"), got)
	}
}

// A run without a conversation has no artifacts: the tool says so, and
// nothing is kept.
func TestArtifactsNeedAConversation(t *testing.T) {
	a, arts := artifactAPI(t)
	run := a.start(`{"profile":"default","input":"make a page"}`)
	a.until(run.ID, func(s Snapshot) bool { return s.State != Running && s.State != Waiting })
	if kinds, _ := a.events(run.ID); slices.Contains(kinds, "artifact") {
		t.Fatal("an artifact without a conversation")
	}
	if list, _ := arts.List(t.Context(), "run-"+run.ID); len(list) != 0 {
		t.Fatal("kept somewhere")
	}
}
