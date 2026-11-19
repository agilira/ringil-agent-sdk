// look.go: what Tool and File triggers see
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"
)

// ErrLook: a trigger could not look at what it watches.
const ErrLook errors.ErrorCode = "TRIGGER_LOOK"

// maxEntries bounds the files of a directory a File trigger looks at, and
// maxListed those a run is told of.
const (
	maxEntries = 10000
	maxListed  = 100
)

// look sees what a Tool or File trigger watches and says whether it
// changed since it last looked (the first look only takes note), and
// what changed.
func (s *Runner) look(ctx context.Context, t Trigger) (detail string, changed bool, err error) {
	var digest string
	target := t.target()
	switch t.kind() {
	case Tool:
		digest, detail, err = s.lookTool(ctx, t)
	case File:
		digest, detail, err = s.lookFile(t)
	}
	if err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status[t.Name]
	if st.Target != target {
		return "", false, nil // edited while it looked: this look is of the old target
	}
	before := st.Seen
	if before == digest {
		return "", false, nil
	}
	st.Seen = digest
	s.status[t.Name] = st
	s.save()
	return detail, before != "", nil
}

func (s *Runner) lookTool(ctx context.Context, t Trigger) (digest, detail string, err error) {
	if s.cfg.Call == nil {
		return "", "", errors.New(ErrLook, "this runner cannot call tools")
	}
	args := t.Args
	var compact bytes.Buffer
	if len(args) > 0 && json.Compact(&compact, args) == nil {
		args = compact.Bytes() // the store indents it
	}
	answer, err := s.cfg.Call(ctx, t.Tool, args)
	if err != nil {
		return "", "", errors.Wrap(err, ErrLook, "tool "+t.Tool+" failed")
	}
	sum := sha256.Sum256([]byte(answer))
	return hex.EncodeToString(sum[:]), answer, nil
}

// snapshot is what a File trigger saw: each file's size and time, by
// name ("" for the path itself when it is a file).
type snapshot map[string]string

func (s *Runner) lookFile(t Trigger) (digest, detail string, err error) {
	now, err := take(t.Path)
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	before, known := s.seen[t.Name]
	s.seen[t.Name] = now
	s.mu.Unlock()
	detail = "The files there now (they changed while the triggers were not running):\n" + list(now)
	if known {
		detail = changes(before, now)
	}
	return now.digest(), "Watched: " + t.Path + "\n" + detail, nil
}

// take looks at path: a file, a directory's own files, or nothing.
func take(path string) (snapshot, error) {
	info, err := os.Stat(path) // the path itself may be a link (/srv/app/current); its files are not followed
	switch {
	case stderrors.Is(err, fs.ErrNotExist):
		return snapshot{}, nil
	case err != nil:
		return nil, errors.Wrap(err, ErrLook, "cannot look at "+path)
	case !info.IsDir():
		return snapshot{"": stamp(info)}, nil
	}
	entries, err := readDir(path)
	if err != nil {
		return nil, err
	}
	if len(entries) > maxEntries {
		return nil, errors.New(ErrLook, path+" has more than 10000 files: watch a smaller directory")
	}
	out := snapshot{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue // gone since it was listed: the next look sees it so
		}
		out[e.Name()] = stamp(info)
	}
	return out, nil
}

// readDir reads at most maxEntries+1 entries of a directory: one more
// tells it has too many, without reading them all.
func readDir(path string) ([]fs.DirEntry, error) {
	f, err := os.Open(path) // #nosec G304 -- the path the trigger watches
	if err != nil {
		return nil, errors.Wrap(err, ErrLook, "cannot read "+path)
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(maxEntries + 1)
	if err != nil && !stderrors.Is(err, io.EOF) {
		return nil, errors.Wrap(err, ErrLook, "cannot read "+path)
	}
	return entries, nil
}

func stamp(info fs.FileInfo) string {
	kind := "file"
	if info.IsDir() {
		kind = "dir"
	}
	return kind + " " + strconv.FormatInt(info.Size(), 10) + " " + info.ModTime().UTC().Format(time.RFC3339Nano)
}

func (sn snapshot) digest() string {
	h := sha256.New()
	for _, name := range sn.names() {
		h.Write([]byte(name + "\x00" + sn[name] + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (sn snapshot) names() []string {
	names := make([]string, 0, len(sn))
	for n := range sn {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// changes says what was created, changed and removed from before to now.
func changes(before, now snapshot) string {
	var lines []string
	for _, n := range now.names() {
		switch was, ok := before[n]; {
		case !ok:
			lines = append(lines, "created: "+shown(n))
		case was != now[n]:
			lines = append(lines, "changed: "+shown(n))
		}
	}
	for _, n := range before.names() {
		if _, ok := now[n]; !ok {
			lines = append(lines, "removed: "+shown(n))
		}
	}
	return listed(lines)
}

// list names the files there are.
func list(sn snapshot) string {
	var lines []string
	for _, n := range sn.names() {
		lines = append(lines, shown(n))
	}
	if len(lines) == 0 {
		return "(none)"
	}
	return listed(lines)
}

func listed(lines []string) string {
	if len(lines) > maxListed {
		lines = append(lines[:maxListed], "… and "+strconv.Itoa(len(lines)-maxListed)+" more")
	}
	return strings.Join(lines, "\n")
}

// shown is a file's name for the run: the path itself for "", quoted
// when it holds what could pass for something else.
func shown(name string) string {
	if name == "" {
		return "(the file itself)"
	}
	if strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return strconv.Quote(name)
	}
	return filepath.Base(name)
}
