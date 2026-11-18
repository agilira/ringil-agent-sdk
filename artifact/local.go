// local.go: artifacts in a private directory
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

// Local keeps each scope in its own folder: an index of versions and the
// contents, one file per SHA-256, written atomically under the scope's
// lock.
type Local struct {
	dir string
	// MaxSize bounds one artifact; default 64 MiB.
	MaxSize int64
	// MaxVersions bounds the versions kept of one name; publishing past it
	// drops the oldest. Default 100.
	MaxVersions int
	// MaxScope bounds the bytes of one scope's contents; default 1 GiB.
	MaxScope int64
}

const (
	indexFile = "index.json"
	lockName  = ".lock"
	blobDir   = "blobs"
)

// NewLocal opens dir, creating it private.
func NewLocal(dir string) (*Local, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot create the artifact directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || lockfile.CheckPrivate(info) != nil {
		return nil, errors.New(ErrIO, "the artifact directory must be a real directory of yours, not writable by others")
	}
	return &Local{dir: dir}, nil
}

func (l *Local) maxVersions() int {
	if l.MaxVersions > 0 {
		return l.MaxVersions
	}
	return 100
}

func (l *Local) maxScope() int64 {
	if l.MaxScope > 0 {
		return l.MaxScope
	}
	return 1 << 30
}

func (l *Local) maxSize() int64 {
	if l.MaxSize > 0 {
		return l.MaxSize
	}
	return 64 << 20
}

type index struct {
	Version   int               `json:"version"`
	Artifacts map[string][]Info `json:"artifacts"`
	// orphans are contents to remove once the index no longer names
	// them: removed only after the index is saved.
	orphans []string
}

// Put implements Store.
func (l *Local) Put(ctx context.Context, scope, name, mediaType, by string, data []byte) (Info, error) {
	if err := check(scope, name); err != nil {
		return Info{}, err
	}
	if !validMedia.MatchString(mediaType) {
		mediaType = "application/octet-stream"
	}
	if int64(len(data)) > l.maxSize() {
		return Info{}, errors.New(ErrInput, "the artifact is too large").WithUserMessage("The artifact is larger than the store allows.")
	}
	sum := sha256.Sum256(data)
	info := Info{Name: name, MediaType: mediaType, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Created: time.Now().UTC(), By: by}
	err := l.locked(ctx, scope, func(root *os.Root, idx *index) (bool, error) {
		if idx.bytes()+info.Size > l.maxScope() {
			return false, errors.New(ErrInput, "the scope is full").WithUserMessage("This scope is full; delete artifacts no longer needed.")
		}
		if err := writeBlob(root, info.SHA256, data); err != nil {
			return false, err
		}
		versions := idx.Artifacts[name]
		info.Version = 1
		if len(versions) > 0 {
			info.Version = versions[len(versions)-1].Version + 1
		}
		versions = append(versions, info)
		if extra := len(versions) - l.maxVersions(); extra > 0 {
			dropped := versions[:extra]
			versions = versions[extra:]
			idx.Artifacts[name] = versions
			idx.orphans = idx.unused(dropped)
		}
		idx.Artifacts[name] = versions
		return true, nil
	})
	return info, err
}

// bytes is the size of the scope's distinct contents.
func (idx *index) bytes() int64 {
	seen := map[string]bool{}
	var n int64
	for _, vs := range idx.Artifacts {
		for _, v := range vs {
			if !seen[v.SHA256] {
				seen[v.SHA256] = true
				n += v.Size
			}
		}
	}
	return n
}

// unused lists the contents of gone that no version in the index uses.
func (idx *index) unused(gone []Info) []string {
	used := map[string]bool{}
	for _, vs := range idx.Artifacts {
		for _, v := range vs {
			used[v.SHA256] = true
		}
	}
	var out []string
	for _, v := range gone {
		if !used[v.SHA256] {
			out = append(out, v.SHA256)
			used[v.SHA256] = true
		}
	}
	return out
}

func writeBlob(root *os.Root, sum string, data []byte) error {
	if err := root.MkdirAll(blobDir, 0o700); err != nil {
		return err
	}
	if have, err := atomicfile.Read(root, blobDir+"/"+sum, int64(len(data))+1); err == nil && digest(have) == sum {
		return nil // the same bytes, intact, already stored
	}
	return atomicfile.Write(root, blobDir+"/"+sum, data) // missing or damaged: written anew
}

func digest(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// Get implements Store.
func (l *Local) Get(ctx context.Context, scope, name string, version int) (Info, []byte, error) {
	if err := check(scope, name); err != nil {
		return Info{}, nil, err
	}
	var info Info
	var data []byte
	err := l.locked(ctx, scope, func(root *os.Root, idx *index) (bool, error) {
		found, ok := pick(idx.Artifacts[name], version)
		if !ok {
			return false, errors.New(ErrNotFound, "no such artifact").WithUserMessage("No artifact " + name + " in this scope with that version.")
		}
		info = found
		var err error
		data, err = readBlob(root, info, l.maxSize())
		return false, err
	})
	return info, data, err
}

// pick finds a version by its number (old ones may have been dropped);
// 0 is the latest.
func pick(versions []Info, version int) (Info, bool) {
	if len(versions) == 0 {
		return Info{}, false
	}
	if version == 0 {
		return versions[len(versions)-1], true
	}
	for _, v := range versions {
		if v.Version == version {
			return v, true
		}
	}
	return Info{}, false
}

// readBlob reads a content and checks it is the one indexed.
func readBlob(root *os.Root, info Info, max int64) ([]byte, error) {
	data, err := atomicfile.Read(root, blobDir+"/"+info.SHA256, max)
	if err != nil {
		return nil, errors.Wrap(err, ErrDamaged, "the artifact's content is missing")
	}
	if digest(data) != info.SHA256 {
		return nil, errors.New(ErrDamaged, "the artifact's content does not match its digest").WithUserMessage("The artifact is damaged.")
	}
	return data, nil
}

// List implements Store.
func (l *Local) List(ctx context.Context, scope string) ([]Info, error) {
	if !validScope.MatchString(scope) {
		return nil, errors.New(ErrInput, "bad scope")
	}
	if _, err := os.Lstat(filepath.Join(l.dir, scope)); os.IsNotExist(err) {
		return nil, nil // nothing published there yet
	}
	var out []Info
	err := l.locked(ctx, scope, func(_ *os.Root, idx *index) (bool, error) {
		for _, versions := range idx.Artifacts {
			out = append(out, versions[len(versions)-1])
		}
		return false, nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

// Delete implements Store: the versions go from the index; contents no
// other version uses are removed.
func (l *Local) Delete(ctx context.Context, scope, name string) error {
	if err := check(scope, name); err != nil {
		return err
	}
	return l.locked(ctx, scope, func(_ *os.Root, idx *index) (bool, error) {
		gone := idx.Artifacts[name]
		if len(gone) == 0 {
			return false, errors.New(ErrNotFound, "no such artifact")
		}
		delete(idx.Artifacts, name)
		idx.orphans = idx.unused(gone)
		return true, nil
	})
}

// locked runs fn on the scope's index under its lock, saving the index
// when fn reports a change.
func (l *Local) locked(ctx context.Context, scope string, fn func(*os.Root, *index) (bool, error)) error {
	top, err := os.OpenRoot(l.dir)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot open the artifact directory")
	}
	defer closeRoot(top)
	// Through the root: names Windows reserves (CON, NUL) are refused.
	if err := top.MkdirAll(scope, 0o700); err != nil {
		return errors.Wrap(err, ErrIO, "cannot open the scope")
	}
	root, err := top.OpenRoot(scope)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot open the scope")
	}
	defer closeRoot(root)
	lock, err := lockfile.Open(root, lockName)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot lock the scope")
	}
	defer closeFile(lock)
	if err := lockfile.Lock(ctx, lock); err != nil {
		return err
	}
	return update(root, fn)
}

func update(root *os.Root, fn func(*os.Root, *index) (bool, error)) error {
	idx, err := readIndex(root)
	if err != nil {
		return err
	}
	changed, err := fn(root, idx)
	if err != nil || !changed {
		return err
	}
	data, err := json.Marshal(idx)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot encode the index")
	}
	if err := atomicfile.Write(root, indexFile, data); err != nil {
		return errors.Wrap(err, ErrIO, "cannot write the index")
	}
	for _, sum := range idx.orphans { // after the index: it never names a missing content
		if err := root.Remove(blobDir + "/" + sum); err != nil && !os.IsNotExist(err) {
			return errors.Wrap(err, ErrIO, "cannot remove an unused content")
		}
	}
	return nil
}

func readIndex(root *os.Root) (*index, error) {
	idx := &index{Version: 1, Artifacts: map[string][]Info{}}
	data, err := atomicfile.Read(root, indexFile, 64<<20)
	if os.IsNotExist(err) {
		return idx, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot read the index")
	}
	if json.Unmarshal(data, idx) != nil || idx.Version != 1 || idx.Artifacts == nil {
		return nil, errors.New(ErrDamaged, "the artifact index is damaged")
	}
	return idx, nil
}

func closeRoot(r *os.Root) {
	if err := r.Close(); err != nil {
		return
	}
}

func closeFile(f *os.File) {
	if err := f.Close(); err != nil {
		return
	}
}
