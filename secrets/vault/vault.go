// vault.go: secrets kept in HashiCorp Vault (KV version 2)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package vault is a [secrets.Store] on a KV version 2 engine of
// HashiCorp Vault (or OpenBao, which speaks the same API): each secret is
// an entry under Mount and Path, named as the secret, holding its value
// in the field "value", so a team sees and rotates them with Vault's own
// tools. Deleting removes the latest version, which Vault keeps
// recoverable (vault kv undelete). Every write names the version it
// replaces (check-and-set), so two writers never overwrite each other
// unseen, and mounts that require it work.
//
// Before its first call the store checks that Mount is a KV version 2
// engine, and with a Namespace that the server is Vault Enterprise (or
// HCP): the open-source server ignores namespaces, and would answer from
// the root one.
//
// The token goes only to Config.URL: redirects to another address (a
// standby node sending to the active one) are refused, so point URL at
// the active node or at the cluster's load balancer.
package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
	"github.com/agilira/ringil-agent-sdk/secrets"
)

// ErrVault: Vault refused the call or did not answer.
const ErrVault errors.ErrorCode = "SECRET_VAULT"

// Bounds of a call.
const (
	maxResponse = 1 << 20 // a value is at most 64 KiB; the rest is Vault's metadata
	callTimeout = 30 * time.Second
	field       = "value"
)

// Config is where the secrets are and who asks.
type Config struct {
	// URL is the Vault server, e.g. https://vault.example.com:8200.
	URL string
	// Mount is the KV version 2 engine's path; default "secret".
	Mount string
	// Path is the folder of these secrets within the mount, e.g.
	// "ringil" or "teams/sales/ringil"; required.
	Path string
	// Token returns the Vault token at each call, so a renewed one is
	// picked up: e.g. secrets.KeyFunc(secrets.Environment{}, "VAULT_TOKEN").
	Token func(ctx context.Context) (string, error)
	// Namespace is the Vault Enterprise or HCP namespace, if any.
	Namespace string
	// AllowHTTP allows plain http to a host that is not the loopback.
	AllowHTTP bool
	// Client is the HTTP client to start from (its TLS settings, proxy).
	Client *http.Client
}

// Vault keeps secrets under one path of a KV version 2 engine.
type Vault struct {
	cfg    Config
	root   string // URL/v1
	client *http.Client

	mu    sync.Mutex
	ready bool // the engine (and the namespace) checked
}

var segments = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)

// New checks cfg; it does not call Vault.
func New(cfg Config) (*Vault, error) {
	if cfg.Mount == "" {
		cfg.Mount = "secret"
	}
	root, err := check(cfg)
	if err != nil {
		return nil, err
	}
	v := &Vault{cfg: cfg, root: root}
	v.client = safehttp.Client(cfg.Client, v.headers, maxResponse, false)
	return v, nil
}

func check(cfg Config) (string, error) {
	root, err := apiRoot(cfg)
	if err != nil {
		return "", err
	}
	if !validPath(cfg.Mount) || !validPath(cfg.Path) {
		return "", errors.New(secrets.ErrSource, "vault: Mount and Path are names of letters, digits, '.', '_' and '-', separated by '/'")
	}
	if cfg.Namespace != "" && !validPath(cfg.Namespace) {
		return "", errors.New(secrets.ErrSource, "vault: the namespace is names separated by '/'")
	}
	if cfg.Token == nil {
		return "", errors.New(secrets.ErrSource, "vault: a Token is required")
	}
	return root, nil
}

// apiRoot is URL/v1, for a URL https (or allowed http), without a query.
func apiRoot(cfg Config) (string, error) {
	if err := safehttp.CheckURL(cfg.URL, cfg.AllowHTTP); err != nil {
		return "", errors.Wrap(err, secrets.ErrSource, "vault: "+err.Error())
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.RawQuery != "" || u.ForceQuery {
		return "", errors.New(secrets.ErrSource, "vault: the URL has no query")
	}
	return u.JoinPath("v1").String(), nil
}

func validPath(p string) bool {
	if !segments.MatchString(p) {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "." || s == ".." {
			return false
		}
	}
	return true
}

// headers are the token's and the namespace's, at each call.
func (v *Vault) headers(ctx context.Context) (http.Header, error) {
	token, err := v.cfg.Token(ctx)
	if err != nil {
		return nil, errors.New(ErrVault, "vault: no token")
	}
	if token == "" || !visible(token) {
		return nil, errors.New(ErrVault, "vault: the token must be visible ASCII")
	}
	h := http.Header{"X-Vault-Token": {token}, "X-Vault-Request": {"true"}}
	if v.cfg.Namespace != "" {
		h.Set("X-Vault-Namespace", v.cfg.Namespace)
	}
	return h, nil
}

func visible(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// entry is a KV version 2 read: the data and its metadata. A 404 has no
// data, and metadata only when the latest version was deleted.
type entry struct {
	Data *struct {
		Data     map[string]any `json:"data"`
		Metadata *struct {
			Version int `json:"version"`
		} `json:"metadata"`
	} `json:"data"`
	Errors []string `json:"errors"`
}

// Lookup implements [secrets.Resolver]: an absent or deleted entry is
// not found; an entry without a "value" field is an error, not absent,
// so a chain does not fall through to another source.
func (v *Vault) Lookup(ctx context.Context, name string) (string, bool, error) {
	if err := secrets.ValidateName(name); err != nil {
		return "", false, err
	}
	e, found, err := v.read(ctx, name)
	if err != nil || !found {
		return "", false, err
	}
	value, ok := e.Data.Data[field].(string)
	if !ok {
		return "", false, errors.New(secrets.ErrValue, "vault: the entry "+name+" has no text field \"value\"")
	}
	return value, true, nil
}

// read is the entry of name; found false when Vault says it is absent or
// deleted, and only then: any other answer is an error.
func (v *Vault) read(ctx context.Context, name string) (entry, bool, error) {
	if err := v.check(ctx); err != nil {
		return entry{}, false, err
	}
	var e entry
	status, err := v.call(ctx, http.MethodGet, v.data(name), nil, &e)
	if err != nil {
		return entry{}, false, err
	}
	if status == http.StatusNotFound {
		return e, false, v.absent(e)
	}
	if e.Data == nil || e.Data.Data == nil || e.Data.Metadata == nil {
		return entry{}, false, errors.New(ErrVault, "vault: an answer that is not Vault's")
	}
	return e, true, nil
}

// absent checks a 404 is Vault's for an absent entry ({"errors":[]}) or a
// deleted one (its metadata), not a route nobody serves.
func (v *Vault) absent(e entry) error {
	if (len(e.Errors) == 0 && e.Data == nil) || (e.Data != nil && e.Data.Metadata != nil) {
		return nil
	}
	return errors.New(ErrVault, "vault: "+v.cfg.Mount+" answered as no KV version 2 engine does")
}

func (v *Vault) data(name string) string {
	return "/" + v.cfg.Mount + "/data/" + v.cfg.Path + "/" + name
}

// Set implements [secrets.Store]: a new version of the entry, naming the
// one it replaces (check-and-set): a concurrent write is an error.
func (v *Vault) Set(ctx context.Context, name, value string) error {
	if err := secrets.ValidateName(name); err != nil {
		return err
	}
	if err := secrets.ValidateValue(name, value); err != nil {
		return err
	}
	e, _, err := v.read(ctx, name)
	if err != nil {
		return err
	}
	body := map[string]any{"options": map[string]int{"cas": e.version()}, "data": map[string]string{field: value}}
	var out entry
	status, err := v.call(ctx, http.MethodPost, v.data(name), body, &out)
	return written(name, status, err, out)
}

// version is the entry's current version (a deleted one counts); 0 when
// there is none.
func (e entry) version() int {
	if e.Data == nil || e.Data.Metadata == nil {
		return 0
	}
	return e.Data.Metadata.Version
}

// written is the error of a write: KV version 2 confirms it with data.
func written(name string, status int, err error, out entry) error {
	switch {
	case err != nil && status == http.StatusBadRequest:
		return errors.New(ErrVault, "vault: the write of "+name+" was refused (400): it may have changed meanwhile (check-and-set); read it and try again")
	case err != nil:
		return err
	case out.Data == nil:
		return errors.New(ErrVault, "vault: the write was not confirmed as KV version 2 confirms it")
	}
	return nil
}

// Delete implements [secrets.Store]: the latest version is deleted (Vault
// keeps it recoverable); an absent entry is no error.
func (v *Vault) Delete(ctx context.Context, name string) error {
	if err := secrets.ValidateName(name); err != nil {
		return err
	}
	if err := v.check(ctx); err != nil {
		return err
	}
	status, err := v.call(ctx, http.MethodDelete, v.data(name), nil, nil)
	if err == nil && status == http.StatusNotFound {
		return errors.New(ErrVault, "vault: "+v.cfg.Mount+" answered as no KV version 2 engine does")
	}
	return err
}

// check makes sure, once, that Mount is a KV version 2 engine (as the
// vault CLI asks it) and, with a Namespace, that the server has them.
func (v *Vault) check(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ready {
		return nil
	}
	if err := v.checkMount(ctx); err != nil {
		return err
	}
	if err := v.checkNamespace(ctx); err != nil {
		return err
	}
	v.ready = true
	return nil
}

// checkMount: Mount is a KV version 2 engine, as the vault CLI asks.
func (v *Vault) checkMount(ctx context.Context) error {
	var mount struct {
		Data struct {
			Type    string            `json:"type"`
			Options map[string]string `json:"options"`
		} `json:"data"`
	}
	status, err := v.call(ctx, http.MethodGet, "/sys/internal/ui/mounts/"+v.cfg.Mount, nil, &mount)
	if err != nil || status != http.StatusOK || mount.Data.Type != "kv" || mount.Data.Options["version"] != "2" {
		if err != nil && !errors.HasCode(err, ErrVault) {
			return err
		}
		return errors.New(ErrVault, "vault: "+v.cfg.Mount+" is not a KV version 2 engine this token can use")
	}
	return nil
}

// checkNamespace: with a Namespace, the server has them.
func (v *Vault) checkNamespace(ctx context.Context) error {
	if v.cfg.Namespace == "" {
		return nil
	}
	var health struct {
		Enterprise bool `json:"enterprise"`
	}
	if _, err := v.call(ctx, http.MethodGet, "/sys/health?standbyok=true&perfstandbyok=true", nil, &health); err != nil {
		return err
	}
	if !health.Enterprise {
		return errors.New(ErrVault, "vault: this server has no namespaces (they are Vault Enterprise's): it would answer from the root one")
	}
	return nil
}

// call sends one request and decodes the answer into out, a 404's too.
// It returns 404 as a status, not an error; any other failure is an error
// with the status alone: Vault's messages may repeat what they were sent.
func (v *Vault) call(ctx context.Context, method, path string, in, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return 0, errors.New(ErrVault, "vault: cannot encode the request")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, v.root+path, body)
	if err != nil {
		return 0, errors.New(ErrVault, "vault: bad request")
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		if errors.HasCode(err, ErrVault) {
			return 0, err // the token's own error
		}
		return 0, errors.New(ErrVault, "vault: no answer from "+v.cfg.URL)
	}
	defer func() { _ = resp.Body.Close() }() // #nosec G104 -- read already
	return decode(resp, out)
}

func decode(resp *http.Response, out any) (int, error) {
	status := resp.StatusCode
	switch {
	case status == http.StatusForbidden:
		return status, errors.New(ErrVault, "vault: the token may not do this (403)")
	case status != http.StatusNotFound && (status < 200 || status > 299):
		return status, errors.New(ErrVault, "vault: answered "+strconv.Itoa(status))
	case out == nil || status == http.StatusNoContent:
		_, _ = io.Copy(io.Discard, resp.Body) // #nosec G104 -- drained for reuse
		return status, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return status, errors.New(ErrVault, "vault: an answer that is not Vault's")
	}
	return status, nil
}
