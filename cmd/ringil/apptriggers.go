// apptriggers.go: the automations an app lists, makes and runs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/tools/files"
	"github.com/agilira/ringil-agent-sdk/trigger"
)

// The triggers' API, mounted with the settings:
//
//	GET    /v1/triggers               every trigger, with how its last run went and when it runs next
//	PUT    /v1/triggers/{name}        a trigger's fields (as create_trigger's, and message and webhook too) make or replace it; a new webhook answers its secret, once
//	DELETE /v1/triggers/{name}        remove it
//	POST   /v1/triggers/{name}/pause  stop it, keeping it
//	POST   /v1/triggers/{name}/resume let it run again
//	POST   /v1/triggers/{name}/run    run it now, whatever sets it off: 202
//
// A change is taken by the running triggers at once.
func (c *cli) mountTriggers(octx *orpheus.Context, srv *serve.Server) {
	routes := map[string]http.HandlerFunc{
		"GET /v1/triggers":                c.listTriggers,
		"PUT /v1/triggers/{name}":         c.putTrigger(octx),
		"DELETE /v1/triggers/{name}":      c.editTrigger(func(s *trigger.Store, n string) error { return s.Remove(n) }),
		"POST /v1/triggers/{name}/pause":  c.editTrigger(func(s *trigger.Store, n string) error { return s.Pause(n, true) }),
		"POST /v1/triggers/{name}/resume": c.editTrigger(func(s *trigger.Store, n string) error { return s.Pause(n, false) }),
		"POST /v1/triggers/{name}/run":    c.runTriggerNow,
	}
	for pattern, h := range routes {
		srv.Mount(pattern, h)
	}
}

// TriggerOut is a trigger as the app shows it: never its secret.
type TriggerOut struct {
	trigger.Trigger
	// HasSecret: a webhook's secret is set (it was shown when made).
	HasSecret bool           `json:"has_secret,omitzero"`
	Status    trigger.Status `json:"status"`
	// Next is when it runs or looks next; zero for one the host sets
	// off, or paused.
	Next time.Time `json:"next,omitzero"`
	// Running: a run of it is going now.
	Running bool `json:"running,omitzero"`
}

func (c *cli) listTriggers(w http.ResponseWriter, _ *http.Request) {
	store, err := c.triggerStore()
	if err != nil {
		triggerFail(w, err)
		return
	}
	defer closeQuietly(store)
	list, err := store.Triggers()
	if err != nil {
		triggerFail(w, err)
		return
	}
	status, err := store.Status()
	if err != nil {
		triggerFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c.triggersOut(list, status))
}

// triggersOut are the triggers as the app shows them, with the runner's
// status when it runs (newer than the file between saves).
func (c *cli) triggersOut(list []trigger.Trigger, status map[string]trigger.Status) []TriggerOut {
	var runner *trigger.Runner
	if c.triggers != nil {
		runner = c.triggers.Load()
	}
	if runner != nil {
		status = runner.Status()
	}
	out := make([]TriggerOut, 0, len(list))
	for _, t := range list {
		o := TriggerOut{Trigger: t, HasSecret: t.Secret != "", Status: status[t.Name]}
		o.Secret = ""
		o.Running = !o.Status.LastFire.IsZero() && o.Status.LastEnd.Before(o.Status.LastFire)
		if runner != nil && !t.Paused {
			o.Next = runner.NextRun(t.Name)
		}
		out = append(out, o)
	}
	return out
}

// triggerFail answers a trigger package error with its status.
func triggerFail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.HasCode(err, trigger.ErrNotFound):
		code = http.StatusNotFound
	case errors.HasCode(err, trigger.ErrConfig), errors.HasCode(err, ErrUsage):
		code = http.StatusBadRequest
	case errors.HasCode(err, trigger.ErrRunning), errors.HasCode(err, trigger.ErrStopped):
		code = http.StatusConflict
	}
	fail(w, code, "TRIGGER", describe(err))
}

// putTrigger makes or replaces a trigger from the app: what the user
// sets, checked as the CLI checks it. A webhook keeps its secret; a new
// one gets one, answered once.
func (c *cli) putTrigger(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		t, ok := decodeTrigger(w, r)
		if !ok {
			return
		}
		t.Name = name
		store, err := c.triggerStore()
		if err != nil {
			triggerFail(w, err)
			return
		}
		defer closeQuietly(store)
		fresh := ""
		if t.Kind == trigger.Webhook {
			t.Secret = keptSecret(store, name)
			if t.Secret == "" {
				t.Secret = rand.Text() + rand.Text()
				fresh = t.Secret
			}
		}
		if err := c.checkAppTrigger(octx, &t); err != nil {
			triggerFail(w, err)
			return
		}
		if err := store.Put(t); err != nil {
			triggerFail(w, err)
			return
		}
		c.reloadTriggers()
		if fresh != "" {
			writeJSON(w, http.StatusCreated, map[string]string{"secret": fresh, "path": trigger.WebhookPath + name,
				"signature_header": trigger.SignatureHeader})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// decodeTrigger reads a trigger's fields: one JSON object of known
// fields, without its name (the path's) or a secret (made here).
func decodeTrigger(w http.ResponseWriter, r *http.Request) (trigger.Trigger, bool) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 2*trigger.MaxInput+1))
	if err != nil || len(data) > 2*trigger.MaxInput {
		fail(w, http.StatusBadRequest, "SETTINGS_INPUT", "the body is too large")
		return trigger.Trigger{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) == nil {
		for k := range fields { // in any case: encoding/json takes "Paused" for "paused"
			if slices.Contains([]string{"name", "secret", "paused"}, strings.ToLower(k)) {
				fail(w, http.StatusBadRequest, "SETTINGS_INPUT", k+" is not set this way (the name is the path's; a webhook's secret is made; pause and resume have their own calls)")
				return trigger.Trigger{}, false
			}
		}
	}
	var t trigger.Trigger
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil || dec.More() {
		fail(w, http.StatusBadRequest, "SETTINGS_INPUT", "the body is not one JSON object of a trigger's fields")
		return trigger.Trigger{}, false
	}
	return t, true
}

// keptSecret is the secret of the webhook named so, "" for none.
func keptSecret(store *trigger.Store, name string) string {
	list, err := store.Triggers()
	if err != nil {
		return ""
	}
	for _, t := range list {
		if t.Name == name && t.Kind == trigger.Webhook {
			return t.Secret
		}
	}
	return ""
}

// checkAppTrigger is the CLI's say on a trigger the app makes: as for
// one of the terminal, and as for one the agent makes (the tool it asks
// only reads, the tools it allows exist, the path is not the agent's own
// state nor credentials). A path is made absolute.
func (c *cli) checkAppTrigger(octx *orpheus.Context, t *trigger.Trigger) error {
	if t.Ended(time.Now()) {
		return errors.New(ErrUsage, "its period is over already (until)")
	}
	if err := c.checkTriggerPath(t); err != nil {
		return err
	}
	if _, err := t.Check(); err != nil {
		return err
	}
	if err := c.checkTriggerProfile(t.Profile); err != nil {
		return err
	}
	if err := c.checkTriggerWorkflow(t.Workflow); err != nil {
		return err
	}
	return c.checkTriggerTools(octx, *t)
}

// checkTriggerPath makes a File trigger's path absolute, and refuses one
// that holds credentials or the agent's own state.
func (c *cli) checkTriggerPath(t *trigger.Trigger) error {
	if t.Kind != trigger.File || t.Path == "" {
		return nil
	}
	abs, err := filepath.Abs(t.Path)
	if err != nil {
		return errors.Wrap(err, ErrUsage, "bad path")
	}
	t.Path = abs
	if files.Excluded(t.Path, nil) || c.inOwnState(t.Path) {
		return errors.New(ErrUsage, "a trigger may not watch "+t.Path+": it holds credentials or the agent's own state")
	}
	return nil
}

// checkTriggerTools: the tool a trigger asks is there and only reads,
// the tools it allows are there.
func (c *cli) checkTriggerTools(octx *orpheus.Context, t trigger.Trigger) error {
	if t.Tool == "" && len(t.Allow) == 0 {
		return nil
	}
	r := c.forRun()
	defer r.shutdown()
	reg, err := r.tools(octx, nil, nil)
	if err != nil {
		return errors.Wrap(err, ErrUsage, "the tools cannot be listed")
	}
	if t.Tool != "" && !readsOnly(reg, t.Tool) {
		return errors.New(ErrUsage, "a trigger looks only with a tool there is and that only reads; "+t.Tool+" is not one")
	}
	for _, name := range t.Allow {
		if !reg.Has(name) {
			return errors.New(ErrUsage, "no such tool: "+name)
		}
	}
	return nil
}

// readsOnly: the tool is there and changes nothing.
func readsOnly(reg agent.Registry, tool string) bool {
	spec, ok := specOf(reg, tool)
	return ok && !spec.Annotations.Destructive && spec.Annotations.ReadOnly
}

// editTrigger changes a trigger in the store, and has the runner take it.
func (c *cli) editTrigger(do func(*trigger.Store, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, err := c.triggerStore()
		if err != nil {
			triggerFail(w, err)
			return
		}
		defer closeQuietly(store)
		if err := do(store, r.PathValue("name")); err != nil {
			triggerFail(w, err)
			return
		}
		c.reloadTriggers()
		w.WriteHeader(http.StatusNoContent)
	}
}

// runTriggerNow runs a trigger now, by hand.
func (c *cli) runTriggerNow(w http.ResponseWriter, r *http.Request) {
	var runner *trigger.Runner
	if c.triggers != nil {
		runner = c.triggers.Load()
	}
	if runner == nil {
		fail(w, http.StatusConflict, "TRIGGER", "serve runs no triggers (serve.no_triggers)")
		return
	}
	if err := runner.RunNow(r.PathValue("name")); err != nil {
		triggerFail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
