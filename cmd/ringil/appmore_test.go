// appmore_test.go: chat apps, memory, Composio, remote agents and the
// restart, through the app's API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
)

// Who talks in the chats is written and read back; a profile for the
// members must serve chats; the tokens are not written here.
func TestAppChannels(t *testing.T) {
	appAPI(t, "models: [openai:m]\nprofiles:\n  team: {tools: [], memory: chat}\n", nil, func(h *harness, _ *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/settings/channels", `{"telegram":{"owners":["111"],"members":["222"],"member_phones":[]},"slack":{"owners":["U1"],"members":[]},"member_profile":"team","max_per_hour":10,"language":"it"}`, http.StatusNoContent)
		fileHas(t, filepath.Join(h.dir, "ringil", "ringil.yaml"), "owners:", "111", "member_profile: team", "max_per_hour: 10")
		waitFor(t, func() bool { return h.c.conf().Serve.MemberProfile == "team" })
		var got ChannelsIO
		if err := json.Unmarshal([]byte(must(t, call, "GET", "/v1/settings/channels", "", http.StatusOK)), &got); err != nil {
			t.Fatal(err)
		}
		if got.Telegram.Owners[0] != "111" || got.Slack.Owners[0] != "U1" || *got.MaxPerHour != 10 || got.Telegram.TokenSet || got.Telegram.Running {
			t.Fatalf("%+v", got)
		}
		must(t, call, "PUT", "/v1/settings/channels", `{"member_profile":"nobody"}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/channels", `{"telegram":{"owners":[],"members":[],"member_phones":[],"token_set":true}}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/channels", `{"replies":"smoke"}`, http.StatusBadRequest)
	})
}

// Where the memory is kept is set; the user's memories are listed, added
// and forgotten.
func TestAppMemory(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, _ *serve.Server, call appCall) {
		if out := must(t, call, "GET", "/v1/settings/memory", "", http.StatusOK); !strings.Contains(out, `"kind":"sqlite"`) {
			t.Fatalf("%s", out)
		}
		must(t, call, "PUT", "/v1/settings/memory", `{"kind":"chroma"}`, http.StatusBadRequest) // no url
		must(t, call, "PUT", "/v1/settings/memory", `{"kind":"chroma","url":"http://localhost:8000","headers":{"x-token":"plain-secret"}}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/memory", `{"kind":"chroma","url":"http://localhost:8000","headers":{"x-token":"${CHROMA_TOKEN}"}}`, http.StatusNoContent)
		fileHas(t, filepath.Join(h.dir, "ringil", "ringil.yaml"), "kind: chroma", "${CHROMA_TOKEN}")
		must(t, call, "PUT", "/v1/settings/memory", `{"kind":"sqlite"}`, http.StatusNoContent)
		waitFor(t, func() bool { return h.c.conf().Memory.Kind == "" })
		var rec struct{ ID, Content string }
		if err := json.Unmarshal([]byte(must(t, call, "POST", "/v1/memories", `{"content":"Antonio prefers short answers"}`, http.StatusCreated)), &rec); err != nil || rec.ID == "" {
			t.Fatalf("%v %+v", err, rec)
		}
		if out := must(t, call, "GET", "/v1/memories?q=answers", "", http.StatusOK); !strings.Contains(out, "short answers") {
			t.Fatalf("%s", out)
		}
		must(t, call, "POST", "/v1/memories", `{"content":"  "}`, http.StatusBadRequest)
		must(t, call, "GET", "/v1/memories?limit=0", "", http.StatusBadRequest)
		must(t, call, "DELETE", "/v1/memories/"+rec.ID, "", http.StatusNoContent)
		if out := must(t, call, "GET", "/v1/memories", "", http.StatusOK); out != "[]\n" {
			t.Fatalf("after forgetting: %s", out)
		}
	})
}

// Composio wants a consumer key; remote agents a real address, headers
// as references, and a card that answers; exposing the main agent and a
// restart are said.
func TestAppComposioRemoteRestart(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", []fake.Step{fake.Reply("x")}, func(h *harness, _ *serve.Server, call appCall) {
		if out := must(t, call, "GET", "/v1/settings/composio", "", http.StatusOK); !strings.Contains(out, `"key_set":false`) {
			t.Fatalf("%s", out)
		}
		must(t, call, "PUT", "/v1/settings/composio", `{"key":"sk-not-composio"}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/remote_agents/warehouse", `{"url":"file:///etc"}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/remote_agents/warehouse", `{"url":"https://a.example","headers":{"Authorization":"Bearer x"}}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/remote_agents/warehouse", `{"url":"http://127.0.0.1:1/a2a"}`, http.StatusBadGateway) // nobody answers
		must(t, call, "PUT", "/v1/settings/remote_agents/warehouse", `{"url":"https://a.example/a2a","disabled":true}`, http.StatusNoContent)
		must(t, call, "PUT", "/v1/settings/a2a", `{"expose":true}`, http.StatusNoContent)
		fileHas(t, filepath.Join(h.dir, "ringil", "ringil.yaml"), "warehouse:", "disabled: true", "a2a: true")
		waitFor(t, func() bool { return h.c.conf().Serve.A2A })
		if out := must(t, call, "GET", "/v1/settings", "", http.StatusOK); !strings.Contains(out, `"restart":"agents, serve"`) {
			t.Fatalf("the restart is not said: %s", out)
		}
		if out := must(t, call, "GET", "/v1/settings/remote_agents", "", http.StatusOK); !strings.Contains(out, `"name":"warehouse"`) || !strings.Contains(out, `"expose":true`) {
			t.Fatalf("%s", out)
		}
		must(t, call, "DELETE", "/v1/settings/remote_agents/warehouse", "", http.StatusNoContent)
		must(t, call, "DELETE", "/v1/settings/remote_agents/warehouse", "", http.StatusNotFound)
		must(t, call, "POST", "/v1/restart", "", http.StatusConflict) // a test's serve cannot start itself again
	})
}
