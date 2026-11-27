// apphooks_api_test.go: the hooks through the app's API, on every system
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/ringil-agent-sdk/serve"
)

// The app reads and writes the hooks; a bad one is refused, the file
// left as it was.
func TestHooksAPI(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, _ *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/settings/hooks", `{"before_tool":[{"tools":["bash"],"command":["/usr/local/bin/check","--strict"],"timeout":"5s","on_error":"block"}]}`, http.StatusNoContent)
		fileHas(t, filepath.Join(h.dir, "ringil", "ringil.yaml"), "before_tool", "/usr/local/bin/check", "on_error: block")
		waitFor(t, func() bool { return len(h.c.conf().Hooks.BeforeTool) == 1 })
		out := must(t, call, "GET", "/v1/settings/hooks", "", http.StatusOK)
		if !strings.Contains(out, `"command":["/usr/local/bin/check","--strict"]`) || !strings.Contains(out, `"after_run":[]`) {
			t.Fatalf("%s", out)
		}
		must(t, call, "PUT", "/v1/settings/hooks", `{"before_run":[{"command":["bin/relative"]}]}`, http.StatusBadRequest)
		fileHas(t, filepath.Join(h.dir, "ringil", "ringil.yaml"), "/usr/local/bin/check")
		must(t, call, "PUT", "/v1/settings/hooks", `{}`, http.StatusNoContent)
		waitFor(t, func() bool { return len(h.c.conf().Hooks.BeforeTool) == 0 })
	})
}
