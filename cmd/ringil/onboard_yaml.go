// onboard_yaml.go: writing the answers into ringil.yaml
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/agilira/go-errors"
	"go.yaml.in/yaml/v3"
)

// write sets the setup's answers in ringil.yaml, keeping everything else
// it holds (profiles, MCP servers, comments): the old file is kept as
// ringil.yaml.bak-<date>-<time>.
func (s *setup) write() error {
	doc, err := readDoc(s.path)
	if err != nil {
		return err
	}
	root := doc.Content[0]
	for _, kv := range s.settings() {
		if kv.value == nil {
			removeKey(root, kv.key)
			continue
		}
		setKey(root, kv.key, kv.value)
	}
	s.mergeSections(root)
	if len(s.owners) > 0 {
		serve := mapAt(root, "serve")
		tg := mapAt(serve, "telegram")
		setKey(tg, "owners", sequence(union(stringsAt(tg, "owners"), s.owners)))
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return errors.Wrap(err, ErrUsage, "cannot write ringil.yaml")
	}
	return replaceFile(s.path, out.Bytes())
}

type keyValue struct {
	key   string
	value *yaml.Node // nil removes the key
}

// settings are the answers as ringil.yaml's keys.
func (s *setup) settings() []keyValue {
	opt := func(v string) *yaml.Node {
		if v == "" {
			return nil
		}
		return scalar(v)
	}
	keyEnv := s.provider.keyName // written only where it is not the kind's default
	if keyEnv == map[string]string{"openai": "RINGIL_API_KEY", "anthropic": "ANTHROPIC_API_KEY"}[s.provider.kind] {
		keyEnv = ""
	}
	return []keyValue{
		{"provider", scalar(s.provider.kind)},
		{"base", opt(s.baseURL)},
		{"model", scalar(s.model)},
		{"key_env", opt(keyEnv)},
		{"instructions", opt(s.prompt)},
		{"embed_model", opt(s.embedModel)},
	}
}

// mergeSections sets, in the sections the setup asks about, what it was
// told, field by field: what else they hold stays (Mem0's headers, a
// collection prefix, Vault's mount, a keyring service; audit 22/11: they
// were replaced whole). An answer of no embedder at Ollama removes that
// section: it would win over the answer.
func (s *setup) mergeSections(root *yaml.Node) {
	merge(mapAt(root, "memory_store"), "kind", s.memory.Kind, "url", s.memory.URL)
	sec := mapAt(root, "secrets")
	merge(sec, "store", s.secrets.Store)
	if s.secrets.Store == "vault" {
		merge(mapAt(sec, "vault"), "url", s.secrets.Vault.URL, "path", s.secrets.Vault.Path)
	}
	if s.embedder.Kind == "" {
		removeKey(root, "embedder")
	} else {
		merge(mapAt(root, "embedder"), "kind", s.embedder.Kind, "url", s.embedder.URL, "model", s.embedder.Model)
	}
	if s.composio {
		s.composioSection(mapAt(root, "mcp_servers"))
	}
	if s.allowHTTP {
		setKey(root, "allow_http", &yaml.Node{Kind: yaml.ScalarNode, Value: "true", Tag: "!!bool"})
	}
}

// merge sets the pairs in m; an empty value removes its key (a Mem0 url
// left from before, when the memory is now SQLite).
func merge(m *yaml.Node, pairs ...string) {
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			removeKey(m, pairs[i])
		} else {
			setKey(m, pairs[i], scalar(pairs[i+1]))
		}
	}
}

func readDoc(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the user's own configuration
	if os.IsNotExist(err) {
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "cannot read "+path)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, errors.Wrap(err, ErrUsage, path+" is not YAML: fix it or move it away, then run setup again")
	}
	if len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New(ErrUsage, path+" is not a map of settings")
	}
	return &doc, nil
}

// replaceFile writes data in place of path, private, the old file kept
// beside it as .bak.
func replaceFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errors.Wrap(err, ErrUsage, "cannot make "+filepath.Dir(path))
	}
	if old, err := os.ReadFile(path); err == nil { // #nosec G304 -- as above
		// A backup of its own for every run: the first, hand-written
		// file is never overwritten (audit 22/11).
		backup := path + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backup, old, 0o600); err != nil { // #nosec G703 -- the user's own configuration (--config or the default)
			return errors.Wrap(err, ErrUsage, "cannot keep the old "+path)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { // #nosec G703 -- as above
		return errors.Wrap(err, ErrUsage, "cannot write "+path)
	}
	if err := os.Rename(tmp, path); err != nil { // #nosec G703 -- as above
		_ = os.Remove(tmp) // #nosec G104 -- the rename's error is the one
		return errors.Wrap(err, ErrUsage, "cannot write "+path)
	}
	return nil
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"} }

func sequence(vs []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode}
	for _, v := range vs {
		n.Content = append(n.Content, scalar(v))
	}
	return n
}

// setKey sets key in the map m, in place when present (its comments stay).
func setKey(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v.HeadComment, v.LineComment = m.Content[i+1].HeadComment, m.Content[i+1].LineComment
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, scalar(key), v)
}

func removeKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// mapAt is the map under key, made when missing or not a map.
func mapAt(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.MappingNode {
			return m.Content[i+1]
		}
	}
	n := &yaml.Node{Kind: yaml.MappingNode}
	setKey(m, key, n)
	return n
}

func stringsAt(m *yaml.Node, key string) []string {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.SequenceNode {
			var out []string
			for _, c := range m.Content[i+1].Content {
				out = append(out, c.Value)
			}
			return out
		}
	}
	return nil
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
