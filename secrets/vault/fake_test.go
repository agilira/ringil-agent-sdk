// fake_test.go: a Vault KV version 2 engine in memory
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testToken = "hvs.test-token"

// fakeVault answers as Vault 1.x does for the paths of one mount.
type fakeVault struct {
	mu       sync.Mutex
	mount    string
	versions map[string][]map[string]any // by path, every version
	deleted  map[string]bool             // latest version deleted
	tokens   []string
	space    []string
	kvVer    string // the mount's KV version; default "2"
	enter    bool   // Vault Enterprise
}

func newFake(t *testing.T) (*fakeVault, string) {
	t.Helper()
	f := &fakeVault{mount: "secret", versions: map[string][]map[string]any{}, deleted: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, r.Header.Get("X-Vault-Token"))
	f.space = append(f.space, r.Header.Get("X-Vault-Namespace"))
	if r.Header.Get("X-Vault-Token") != testToken {
		http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
		return
	}
	if f.sys(w, r) {
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/v1/"+f.mount+"/data/")
	if !ok {
		http.Error(w, `{"errors":["no handler for route"]}`, http.StatusNotFound)
		return
	}
	f.data(w, r, path)
}

// sys answers the engine and health checks.
func (f *fakeVault) sys(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/sys/internal/ui/mounts/" + f.mount:
		ver := f.kvVer
		if ver == "" {
			ver = "2"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"type": "kv", "options": map[string]string{"version": ver}}})
		return true
	case "/v1/sys/health":
		_ = json.NewEncoder(w).Encode(map[string]any{"enterprise": f.enter})
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/v1/sys/internal/ui/mounts/") {
		http.Error(w, `{"errors":["preflight capability check returned 403"]}`, http.StatusForbidden)
		return true
	}
	return false
}

// data answers the entries.
func (f *fakeVault) data(w http.ResponseWriter, r *http.Request, path string) {
	switch r.Method {
	case http.MethodGet:
		f.get(w, path)
	case http.MethodPost, http.MethodPut:
		f.write(w, r, path)
	case http.MethodDelete:
		f.deleted[path] = true
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeVault) write(w http.ResponseWriter, r *http.Request, path string) {
	{
		var in struct {
			Options struct {
				CAS *int `json:"cas"`
			} `json:"options"`
			Data map[string]any `json:"data"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, `{"errors":["bad json"]}`, http.StatusBadRequest)
			return
		}
		if in.Options.CAS == nil || *in.Options.CAS != len(f.versions[path]) {
			http.Error(w, `{"errors":["check-and-set parameter did not match the current version"]}`, http.StatusBadRequest)
			return
		}
		f.versions[path] = append(f.versions[path], in.Data)
		f.deleted[path] = false
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": len(f.versions[path])}})
	}
}

// get: the latest version, or 404 (with metadata, as Vault does, when the
// latest was deleted).
func (f *fakeVault) get(w http.ResponseWriter, path string) {
	vs := f.versions[path]
	if len(vs) == 0 {
		http.Error(w, `{"errors":[]}`, http.StatusNotFound)
		return
	}
	if f.deleted[path] {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": nil, "metadata": map[string]any{"version": len(vs), "deletion_time": "2026-11-22T00:00:00Z"}}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": vs[len(vs)-1], "metadata": map[string]any{"version": len(vs)}}})
}

func token(context.Context) (string, error) { return testToken, nil }

func openFake(t *testing.T, url string, cfg Config) *Vault {
	t.Helper()
	cfg.URL = url
	if cfg.Path == "" {
		cfg.Path = "ringil"
	}
	if cfg.Token == nil {
		cfg.Token = token
	}
	v, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
