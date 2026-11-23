// onboard_yaml.go: writing the answers into ringil.yaml
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/agilira/argus"
	"github.com/agilira/go-errors"
)

// write sets the setup's answers in ringil.yaml through argus's writer,
// which edits the file as its document: everything else it holds
// (profiles, MCP servers, comments, order) stays as written. The old file
// is kept as ringil.yaml.bak-<date>-<time>.
func (s *setup) write() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return errors.Wrap(err, ErrUsage, "cannot make "+filepath.Dir(s.path))
	}
	w, err := argus.NewConfigWriter(s.path, argus.FormatYAML, nil)
	if err != nil {
		return errors.Wrap(err, ErrUsage, s.path+" cannot be edited ("+describe(err)+"): fix it or move it away, then run setup again")
	}
	if err := leftovers(w); err != nil {
		return err
	}
	if err := s.answer(w); err != nil {
		return err
	}
	if !w.HasChanges() {
		return nil // the same answers: no write, no backup
	}
	if err := keepBackup(s.path); err != nil {
		return err
	}
	if err := w.WriteConfig(); err != nil {
		return errors.Wrap(err, ErrUsage, "cannot write "+s.path+" ("+describe(err)+")")
	}
	s.dropOAuthToken()
	return nil
}

// answer sets the answers in w: what the setup asked, the sections it
// merges, the bot's owners added to those there.
func (s *setup) answer(w *argus.ConfigWriter) error {
	e := &edit{w: w}
	for _, kv := range s.settings() {
		e.set(kv.key, kv.value)
	}
	s.mergeSections(e)
	if len(s.owners) > 0 {
		e.set("serve.telegram.owners", union(stringsOf(w.GetValue("serve.telegram.owners")), s.owners))
	}
	if e.err != nil {
		return errors.Wrap(e.err, ErrUsage, "cannot set the answers in "+s.path+" ("+describe(e.err)+")")
	}
	return nil
}

// leftovers: base or key_env still in a profile or an audio model, which
// the setup does not ask about (audit 23/11: it said "set up" and the next
// run refused the file). The top level's it replaces.
func leftovers(w *argus.ConfigWriter) error {
	profiles, _ := w.GetValue("profiles").(map[string]interface{})
	for _, name := range slices.Sorted(maps.Keys(profiles)) {
		if p, ok := profiles[name].(map[string]interface{}); ok && (p["base"] != nil || p["key_env"] != nil) {
			return errGone("profiles." + name + ": ")
		}
	}
	for _, where := range []string{"transcribe", "speech"} {
		if w.GetValue(where+".base") != nil || w.GetValue(where+".key_env") != nil {
			return errGone(where + ": ")
		}
	}
	return nil
}

// edit sets and removes values, keeping the first error.
type edit struct {
	w   *argus.ConfigWriter
	err error
}

// set sets key to value; nil or "" removes it.
func (e *edit) set(key string, value interface{}) {
	if e.err != nil {
		return
	}
	if value == nil || value == "" {
		if !e.w.DeleteValue(key) && e.w.GetValue(key) != nil {
			// There, but argus would not remove it (an alias, a key merged
			// with <<): left, the run would use the old value (audit 22/11).
			e.err = errors.New(ErrUsage, key+" is an alias or merged from another entry: change it by hand")
		}
		return
	}
	e.err = e.w.SetValue(key, value)
}

// merge sets the pairs under prefix; an empty value removes its key (a
// Mem0 url left from before, when the memory is now SQLite).
func (e *edit) merge(prefix string, pairs ...string) {
	for i := 0; i+1 < len(pairs); i += 2 {
		e.set(prefix+"."+pairs[i], pairs[i+1])
	}
}

type keyValue struct {
	key   string
	value interface{} // "" removes the key
}

// settings are the answers as ringil.yaml's keys: the provider by name,
// and in the providers section a server of one's own, or a built-in at
// another address than its own.
func (s *setup) settings() []keyValue {
	kv := []keyValue{
		{"provider", s.endpoint.name},
		{"base", ""}, // the endpoint is the provider's: none beside it
		{"key_env", ""},
		{"model", s.model},
		{"instructions", s.prompt},
		{"embed_model", s.embedModel},
	}
	if !s.custom && s.endpoint.base == builtinProviders[s.endpoint.name].Base {
		// A built-in at its own address: an entry of the file that sent its
		// key elsewhere goes (audit 23/11: the key checked here went there).
		return append(kv, keyValue{"providers." + s.endpoint.name, ""})
	}
	entry := map[string]interface{}{"base": s.endpoint.base}
	if s.endpoint.keyName != "" {
		entry["key_env"] = s.endpoint.keyName
	}
	return append(kv, keyValue{"providers." + s.endpoint.name, entry})
}

// mergeSections sets, in the sections the setup asks about, what it was
// told, field by field: what else they hold stays (Mem0's headers, a
// collection prefix, Vault's mount, a keyring service; audit 22/11: they
// were replaced whole). An answer of no embedder at Ollama removes that
// section: it would win over the answer.
func (s *setup) mergeSections(e *edit) {
	e.merge("memory_store", "kind", s.memory.Kind, "url", s.memory.URL)
	e.merge("secrets", "store", s.secrets.Store)
	if s.secrets.Store == "vault" {
		e.merge("secrets.vault", "url", s.secrets.Vault.URL, "path", s.secrets.Vault.Path)
	}
	if s.embedder.Kind == "" {
		e.set("embedder", nil)
	} else {
		e.merge("embedder", "kind", s.embedder.Kind, "url", s.embedder.URL, "model", s.embedder.Model)
	}
	if s.composio {
		s.composioSection(e)
	}
	if s.allowHTTP {
		e.set("allow_http", true)
	}
}

// keepBackup copies the file, if any, beside it: a backup of its own for
// every run, so the first, hand-written file is never overwritten (audit
// 22/11).
func keepBackup(path string) error {
	old, err := os.ReadFile(path) // #nosec G304 -- the user's own configuration (--config or the default)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.Wrap(err, ErrUsage, "cannot read "+path)
	}
	backup := path + ".bak-" + time.Now().Format("20060102-150405")
	if err := os.WriteFile(backup, old, 0o600); err != nil { // #nosec G703 -- as above
		return errors.Wrap(err, ErrUsage, "cannot keep the old "+path)
	}
	return nil
}

// stringsOf is a list of strings as the writer gives it; numbers are
// their text (owners: [111, 222], as the loader accepts them: dropped, the
// setup lost them, audit 22/11).
func stringsOf(v interface{}) []string {
	items, _ := v.([]interface{})
	var out []string
	for _, item := range items {
		switch x := item.(type) {
		case string:
			out = append(out, x)
		case int, int64, uint64:
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

func union(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, v := range b {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
