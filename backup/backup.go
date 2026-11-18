// backup.go: archives of an agent's state, verified and restored apart
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package backup saves an agent's state (sessions, memory, notes,
// workflow journals, artifacts: whatever directories the host names) into
// one gzip'd tar with a manifest of SHA-256 digests, checks an archive
// without extracting it, and restores it into a new directory only.
//
// A restore never overwrites and never activates: the state lands in a
// fresh directory marked QUARANTINE, because an old snapshot says nothing
// of what happened since (effects already done, money already spent). The
// host or the user decides what to do with it.
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
	// QuarantineFile marks a restored directory.
	QuarantineFile = "QUARANTINE.md"
	maxEntry       = 1 << 30
)

// Manifest lists an archive's files and their digests.
type Manifest struct {
	Version int               `json:"version"`
	Created time.Time         `json:"created"`
	Sources []string          `json:"sources"`
	Files   map[string]string `json:"files"` // archive path -> sha256
}

var sourceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Create writes the directories of sources (name -> directory) into w.
// Regular files only; links, sockets and lock files are left out, as are
// names the skip function refuses. A missing directory is skipped.
func Create(ctx context.Context, w io.Writer, sources map[string]string, skip func(rel string) bool) (*Manifest, error) {
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

func addSource(ctx context.Context, tw *tar.Writer, m *Manifest, name, dir string, skip func(string) bool) error {
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

func isLock(p string) bool {
	base := path.Base(p)
	return strings.HasSuffix(base, ".lock") || base == ".lock"
}

func addFile(tw *tar.Writer, m *Manifest, root *os.Root, archived, p string) error {
	f, err := root.Open(p)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot read "+archived)
	}
	defer closeFile(f)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil // changed under us into something else: left out
	}
	hdr := &tar.Header{Name: archived, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		return errors.Wrap(err, ErrIO, "cannot write the archive")
	}
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(tw, h), f, info.Size()); err != nil {
		return errors.Wrap(err, ErrIO, archived+" changed while it was saved; try again")
	}
	m.Files[archived] = hex.EncodeToString(h.Sum(nil))
	return nil
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
