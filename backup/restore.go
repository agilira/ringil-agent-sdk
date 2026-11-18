// restore.go: checking an archive and restoring it apart
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/agilira/go-errors"
)

// Verify reads a whole archive and checks every file against the
// manifest: same files, same digests, nothing extra.
func Verify(ctx context.Context, r io.Reader) (*Manifest, error) {
	return walk(ctx, r, func(string, io.Reader) error { return nil })
}

// walk reads the archive, hands every file to each, and checks the
// digests against the manifest at the end.
func walk(ctx context.Context, r io.Reader, each func(name string, body io.Reader) error) (*Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, errors.Wrap(err, ErrDamaged, "not a backup archive")
	}
	defer closeFile(gz)
	tr := tar.NewReader(gz)
	seen := map[string]string{}
	var m *Manifest
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.Wrap(err, ErrDamaged, "the archive is damaged")
		}
		if hdr.Name == manifestName {
			if m, err = readManifest(tr, hdr.Size); err != nil {
				return nil, err
			}
			continue
		}
		sum, err := entry(hdr, tr, each)
		if err != nil {
			return nil, err
		}
		seen[hdr.Name] = sum
	}
	return m, match(m, seen)
}

func entry(hdr *tar.Header, tr io.Reader, each func(string, io.Reader) error) (string, error) {
	if hdr.Typeflag != tar.TypeReg || !safePath(hdr.Name) || hdr.Size < 0 || hdr.Size > maxEntry {
		return "", errors.New(ErrDamaged, "the archive holds an entry it should not: "+strings.ToValidUTF8(hdr.Name, "?"))
	}
	h := sha256.New()
	body := io.TeeReader(io.LimitReader(tr, hdr.Size), h)
	if err := each(hdr.Name, body); err != nil {
		return "", err
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return "", errors.Wrap(err, ErrDamaged, "the archive is cut short")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// safePath accepts relative slash paths of plain components.
func safePath(p string) bool {
	return fs.ValidPath(p) && p != "." && !strings.Contains(p, "\\") && path.Clean(p) == p
}

func readManifest(r io.Reader, size int64) (*Manifest, error) {
	if size > 64<<20 {
		return nil, errors.New(ErrDamaged, "the manifest is too large")
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(r, size)).Decode(&m); err != nil || m.Version != 1 || m.Files == nil {
		return nil, errors.New(ErrDamaged, "the manifest is damaged")
	}
	return &m, nil
}

func match(m *Manifest, seen map[string]string) error {
	if m == nil {
		return errors.New(ErrDamaged, "the archive has no manifest")
	}
	if len(seen) != len(m.Files) {
		return errors.New(ErrDamaged, "the archive's files do not match its manifest")
	}
	for name, sum := range m.Files {
		if seen[name] != sum {
			return errors.New(ErrDamaged, "the archive's file "+name+" does not match its digest")
		}
	}
	return nil
}

// Restore extracts an archive into dest, which must not exist. The files
// go to a hidden building directory first; only a fully verified archive
// is renamed into place, with a QUARANTINE.md that says what it is.
func Restore(ctx context.Context, r io.Reader, dest string) (*Manifest, error) {
	if _, err := os.Lstat(dest); err == nil {
		return nil, errors.New(ErrExists, dest+" already exists: restore into a new directory")
	}
	building := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+"-building-"+strings.ToLower(rand.Text()[:8]))
	if err := os.MkdirAll(building, 0o700); err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot create the restore directory")
	}
	m, err := extract(ctx, r, building)
	if err == nil {
		err = finishRestore(building, dest, m)
	}
	if err != nil {
		if rerr := os.RemoveAll(building); rerr != nil {
			return nil, errors.Wrap(err, ErrIO, "restore failed and left "+building)
		}
		return nil, err
	}
	return m, nil
}

func extract(ctx context.Context, r io.Reader, dir string) (*Manifest, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot open the restore directory")
	}
	defer closeRoot(root)
	return walk(ctx, r, func(name string, body io.Reader) error {
		if d := path.Dir(name); d != "." {
			if err := root.MkdirAll(d, 0o700); err != nil {
				return errors.Wrap(err, ErrIO, "cannot restore "+name)
			}
		}
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return errors.Wrap(err, ErrDamaged, "the archive repeats or misplaces "+name)
		}
		_, err = io.Copy(f, body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return errors.Wrap(err, ErrIO, "cannot restore "+name)
		}
		return nil
	})
}

func finishRestore(building, dest string, m *Manifest) error {
	note := "# Restored state, in quarantine\n\nRestored on " + time.Now().UTC().Format(time.RFC3339) +
		" from a backup made on " + m.Created.Format(time.RFC3339) + ".\n\n" +
		"Nothing uses this directory. A snapshot says nothing of what happened after it: work it marks as pending may have been done, money it counts as unspent may be spent. Check before using any of it.\n"
	if err := os.WriteFile(filepath.Join(building, QuarantineFile), []byte(note), 0o600); err != nil {
		return errors.Wrap(err, ErrIO, "cannot mark the quarantine")
	}
	if err := os.Rename(building, dest); err != nil {
		return errors.Wrap(err, ErrIO, "cannot move the restored state into place")
	}
	return nil
}
