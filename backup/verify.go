// verify.go: reading an archive and checking it against its manifest
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

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
	"path"
	"strconv"
	"strings"
	"unicode"

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
	w := &walker{tr: tar.NewReader(gz), seen: map[string]string{}, each: each}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := w.tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.Wrap(err, ErrDamaged, "the archive is damaged")
		}
		if err := w.next(hdr); err != nil {
			return nil, err
		}
	}
	return w.m, match(w.m, w.seen)
}

type walker struct {
	tr    *tar.Reader
	seen  map[string]string
	m     *Manifest
	total int64
	files int
	each  func(string, io.Reader) error
}

// next takes one entry: the manifest, which must come last, or a file
// seen once, within the total bound.
func (w *walker) next(hdr *tar.Header) error {
	var err error
	switch {
	case w.m != nil:
		return errors.New(ErrDamaged, "the archive goes on after its manifest")
	case hdr.Name == manifestName:
		w.m, err = readManifest(w.tr, hdr)
		return err
	case w.seen[hdr.Name] != "":
		return errors.New(ErrDamaged, "the archive repeats "+strconv.Quote(hdr.Name))
	}
	if err := w.count(hdr); err != nil {
		return err
	}
	sum, err := entry(hdr, w.tr, w.each)
	if err != nil {
		return err
	}
	w.seen[hdr.Name] = sum
	return nil
}

func (w *walker) count(hdr *tar.Header) error {
	if w.files++; w.files > maxFiles {
		return errors.New(ErrDamaged, "the archive holds more files than an import allows")
	}
	if hdr.Size < 0 || hdr.Size > maxEntry {
		return errors.New(ErrDamaged, "the archive holds an entry it should not: "+strconv.Quote(hdr.Name))
	}
	if hdr.Size > MaxTotal-w.total {
		return errors.New(ErrDamaged, "the archive is larger than a restore allows")
	}
	w.total += hdr.Size
	return nil
}

func entry(hdr *tar.Header, tr io.Reader, each func(string, io.Reader) error) (string, error) {
	if hdr.Typeflag != tar.TypeReg || !safePath(hdr.Name) || hdr.Size < 0 || hdr.Size > maxEntry {
		return "", errors.New(ErrDamaged, "the archive holds an entry it should not: "+strconv.Quote(hdr.Name))
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

// safePath accepts relative slash paths of plain components, without
// control characters.
func safePath(p string) bool {
	return fs.ValidPath(p) && p != "." && !strings.Contains(p, "\\") && path.Clean(p) == p &&
		!strings.ContainsFunc(p, unicode.IsControl)
}

// maxFiles bounds the files of one archive.
const maxFiles = 1 << 20

// MaxTotal bounds what one import writes: a small archive cannot fill
// the disk.
var MaxTotal int64 = 16 << 30

func readManifest(r io.Reader, hdr *tar.Header) (*Manifest, error) {
	switch {
	case hdr.Typeflag != tar.TypeReg || hdr.Size < 0:
		return nil, errors.New(ErrDamaged, "the manifest is damaged")
	case hdr.Size > 64<<20:
		return nil, errors.New(ErrDamaged, "the manifest is too large")
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(r, hdr.Size)).Decode(&m); err != nil || m.Version != 1 || m.Files == nil {
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
			return errors.New(ErrDamaged, "the archive's file "+strconv.Quote(name)+" does not match its digest")
		}
	}
	return nil
}
