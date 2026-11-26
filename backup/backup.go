// backup.go: exporting an agent's state into one archive
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package backup exports an agent's state (sessions, memory, notes,
// workflows, artifacts, settings: whatever directories and files the host
// names) into one gzip'd tar with a manifest of SHA-256 digests, which
// the user saves where they want; checks an archive without extracting
// it; and imports one back in place of the state.
//
// An import is all or nothing: the archive is extracted beside each
// place and checked whole first, then every place is swapped at once,
// and a failed swap puts back what was there. It runs only when no
// process uses the state: each takes a shared lock with Use, an import
// the exclusive one. The host keeps a copy of the state before (an
// export): an old snapshot says nothing of what happened since.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agilira/go-errors"
)

// Error codes.
const (
	ErrInput   errors.ErrorCode = "BACKUP_INPUT"
	ErrIO      errors.ErrorCode = "BACKUP_IO"
	ErrDamaged errors.ErrorCode = "BACKUP_DAMAGED"
	ErrExists  errors.ErrorCode = "BACKUP_EXISTS"
)

const (
	manifestName = "MANIFEST.json"
	maxEntry     = 1 << 30
)

// Manifest lists an archive's files and their digests.
type Manifest struct {
	Version int               `json:"version"`
	Created time.Time         `json:"created"`
	Sources []string          `json:"sources"`
	Files   map[string]string `json:"files"` // archive path -> sha256
}

var sourceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Source is one part of the state: a directory, or a single file.
type Source struct {
	Path string
	File bool
}

// Create writes sources into w: a directory's files as name/<path>, a
// file as name. Stores are read file
// by file, not frozen: export while no run is going (a server pauses
// them) for a consistent copy. Regular files only; links, sockets and
// lock files are left out, as are names the skip function refuses. A
// missing source is skipped, and not in the manifest's Sources.
func Create(ctx context.Context, w io.Writer, sources map[string]Source, skip func(rel string) bool) (*Manifest, error) {
	m := &Manifest{Version: 1, Created: time.Now().UTC(), Files: map[string]string{}}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(sources))
	for n := range sources {
		if !sourceName.MatchString(n) {
			return nil, errors.New(ErrInput, "source names are lowercase words: "+n)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := addSource(ctx, tw, m, n, sources[n], skip); err != nil {
			return nil, err
		}
	}
	if err := writeManifest(tw, m); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot finish the archive")
	}
	if err := gz.Close(); err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot finish the archive")
	}
	return m, nil
}

func addSource(ctx context.Context, tw *tar.Writer, m *Manifest, name string, src Source, skip func(string) bool) error {
	info, err := os.Stat(src.Path) // a source that is a link is followed: the host named it
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return errors.Wrap(err, ErrIO, "cannot read "+name)
	case src.File && info.Mode().IsRegular():
		return addOne(tw, m, name, src.Path)
	case !src.File && info.IsDir():
		return addDir(ctx, tw, m, name, src.Path, skip)
	}
	return errors.New(ErrInput, name+" is not a "+src.kind()+" (a link, or the other kind): it cannot be exported as it is")
}

func (s Source) kind() string {
	if s.File {
		return "regular file"
	}
	return "directory"
}

func addDir(ctx context.Context, tw *tar.Writer, m *Manifest, name, dir string, skip func(string) bool) error {
	root, err := os.OpenRoot(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot open "+name)
	}
	defer closeRoot(root)
	m.Sources = append(m.Sources, name)
	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return errors.Wrap(err, ErrIO, "cannot read "+name+"/"+p)
		case ctx.Err() != nil:
			return ctx.Err()
		case !d.Type().IsRegular() || isLock(p) || (skip != nil && skip(name+"/"+p)):
			return nil
		}
		return addFile(tw, m, root, name+"/"+p, p)
	})
}

// isLock: lock files and the temporary files of atomic writes are not
// state.
func isLock(p string) bool {
	base := path.Base(p)
	return strings.HasSuffix(base, ".lock") || base == ".lock" || (strings.HasPrefix(base, ".") && strings.HasSuffix(base, ".tmp"))
}

// addOne adds a file source, archived as its name: the file itself,
// through a link if it is one (a settings file kept with dotfiles).
func addOne(tw *tar.Writer, m *Manifest, name, file string) error {
	f, err := os.Open(file) // #nosec G304 -- a source the host named
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot read "+name)
	}
	defer closeFile(f)
	added, err := addOpen(tw, m, name, f)
	if err == nil && added {
		m.Sources = append(m.Sources, name)
	}
	return err
}

func addFile(tw *tar.Writer, m *Manifest, root *os.Root, archived, p string) error {
	f, err := root.Open(p)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot read "+archived)
	}
	defer closeFile(f)
	_, err = addOpen(tw, m, archived, f)
	return err
}

// addOpen archives an open regular file; false when it is not one any
// more (changed under us: left out).
func addOpen(tw *tar.Writer, m *Manifest, archived string, f *os.File) (bool, error) {
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	hdr := &tar.Header{Name: archived, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		return false, errors.Wrap(err, ErrIO, "cannot write the archive")
	}
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(tw, h), f, info.Size()); err != nil {
		return false, errors.Wrap(err, ErrIO, archived+" changed while it was saved; try again")
	}
	m.Files[archived] = hex.EncodeToString(h.Sum(nil))
	return true, nil
}

func writeManifest(tw *tar.Writer, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot encode the manifest")
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: m.Created}); err != nil {
		return errors.Wrap(err, ErrIO, "cannot write the manifest")
	}
	if _, err := tw.Write(data); err != nil {
		return errors.Wrap(err, ErrIO, "cannot write the manifest")
	}
	return nil
}

func closeRoot(r *os.Root) {
	if err := r.Close(); err != nil {
		return
	}
}

func closeFile(f io.Closer) {
	if err := f.Close(); err != nil {
		return
	}
}
