// install.go: installing and removing skills
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/agilira/go-errors"
)

const (
	manifestName = ".skill.json"
	skillFile    = "SKILL.md"
	stagePrefix  = ".install-"
)

// manifest is what Install approved: the skill's info and a SHA-256 for
// every file. Reads and runs check files against it.
type manifest struct {
	Version int               `json:"version"`
	Info    Info              `json:"info"`
	Files   map[string]string `json:"files"`
	// Agent marks a skill the agent wrote (create_skill): the only kind
	// it may replace.
	Agent bool `json:"agent,omitempty"`
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Install copies the skill in directory from into the catalog directory
// dir, under the name its SKILL.md declares. Only regular files and
// directories are copied; symbolic links and special files are refused,
// hidden entries (.git, .DS_Store) are left out. An installed skill is
// never overwritten: Remove it first to update it.
func Install(ctx context.Context, from, dir string) (Info, error) {
	src, err := os.OpenRoot(from)
	if err != nil {
		return Info{}, errors.Wrap(err, ErrIO, "cannot open the skill directory")
	}
	defer closeQuietly(src)
	data, err := src.ReadFile(skillFile)
	if err != nil {
		return Info{}, errors.Wrap(err, ErrInvalid, "the skill directory has no readable SKILL.md")
	}
	info, _, err := parse(string(data))
	if err != nil {
		return Info{}, err
	}
	root, err := openCatalog(dir)
	if err != nil {
		return Info{}, err
	}
	defer closeQuietly(root)
	return info, publish(ctx, src, root, info, false)
}

// openCatalog opens the catalog directory, creating it private.
func openCatalog(dir string) (*os.Root, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot create the skills directory")
	}
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot open the skills directory")
	}
	tidy(root)
	return root, nil
}

// leftoverAge is how old a staging or replaced directory must be to be
// a leftover of an interrupted install or replace, not one under way.
const leftoverAge = 10 * time.Minute

// tidy clears what interrupted installs and replaces left: a staging
// directory goes; a skill moved aside for a replace that never came back
// is put back in its place, else it goes.
func tidy(root *os.Root) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		info, err := e.Info()
		if err != nil || !e.IsDir() || time.Since(info.ModTime()) < leftoverAge {
			continue
		}
		switch {
		case strings.HasPrefix(name, stagePrefix):
			_ = root.RemoveAll(name)
		case strings.HasPrefix(name, replacedPrefix):
			restore(root, name)
		}
	}
}

// restore puts a skill moved aside back, unless its name is taken again.
func restore(root *os.Root, aside string) {
	var m manifest
	data, err := root.ReadFile(aside + "/" + manifestName)
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if err == nil && validName(m.Info.Name) {
		if _, taken := root.Lstat(m.Info.Name); taken != nil && root.Rename(aside, m.Info.Name) == nil {
			return
		}
	}
	_ = root.RemoveAll(aside)
}

// publish copies into a staging directory and renames it into place, so
// the catalog only ever shows complete skills.
func publish(ctx context.Context, src, root *os.Root, info Info, byAgent bool) error {
	if _, err := root.Lstat(info.Name); err == nil {
		return errors.New(ErrExists, "skill "+info.Name+" is already installed; remove it first")
	}
	stage := stagePrefix + strings.ToLower(rand.Text()[:12])
	if err := root.Mkdir(stage, 0o700); err != nil {
		return errors.Wrap(err, ErrIO, "cannot stage the skill")
	}
	err := stageSkill(ctx, src, root, stage, info, byAgent)
	if err == nil {
		err = root.Rename(stage, info.Name)
	}
	if err != nil {
		failed := installError(err)
		if rerr := root.RemoveAll(stage); rerr != nil {
			return failed.WithContext("cleanup_error", rerr.Error())
		}
		return failed
	}
	return nil
}

// installError keeps the reason of a refusal (a link, a special file) and
// wraps anything else.
func installError(err error) *errors.Error {
	var coded *errors.Error
	if stderrors.As(err, &coded) && coded.Code == ErrInvalid {
		return coded
	}
	return errors.Wrap(err, ErrIO, "cannot install the skill")
}

func stageSkill(ctx context.Context, src, root *os.Root, stage string, info Info, byAgent bool) error {
	dst, err := root.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer closeQuietly(dst)
	m := manifest{Version: 1, Info: info, Files: map[string]string{}, Agent: byAgent}
	walkErr := fs.WalkDir(src.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return copyEntry(src, dst, p, d, m.Files)
	})
	if walkErr != nil {
		return walkErr
	}
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return dst.WriteFile(manifestName, data, 0o600)
}

func copyEntry(src, dst *os.Root, p string, d fs.DirEntry, files map[string]string) error {
	switch {
	case p == ".":
		return nil
	case strings.HasPrefix(path.Base(p), "."):
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	case d.IsDir():
		return dst.Mkdir(p, 0o700)
	case !d.Type().IsRegular():
		return errors.New(ErrInvalid, "the skill holds a link or special file: "+p)
	}
	data, err := readRegular(src, p)
	if err != nil {
		return err
	}
	files[p] = checksum(data)
	return dst.WriteFile(p, data, 0o600)
}

// readRegular reads p only if, once opened, it is still a regular file.
func readRegular(root *os.Root, p string) ([]byte, error) {
	f, err := root.OpenFile(p, os.O_RDONLY|nonBlock, 0)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(f)
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || linked(st) {
		return nil, errors.New(ErrInvalid, "not a regular file with a single link: "+p)
	}
	return io.ReadAll(f)
}

// Remove deletes an installed skill from the catalog directory dir.
func Remove(dir, name string) error {
	if !validName(name) {
		return errors.New(ErrNotFound, "no skill named "+name)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot open the skills directory")
	}
	defer closeQuietly(root)
	if _, err := root.Lstat(name); err != nil {
		return errors.New(ErrNotFound, "no skill named "+name)
	}
	// Rename first: the skill leaves the catalog at once, and an
	// interrupted removal leaves only a hidden directory behind.
	gone := ".remove-" + strings.ToLower(rand.Text()[:12])
	if err := root.Rename(name, gone); err != nil {
		return errors.Wrap(err, ErrIO, "cannot remove the skill")
	}
	if err := root.RemoveAll(gone); err != nil {
		return errors.Wrap(err, ErrIO, "the skill is removed but its files remain in "+gone)
	}
	return nil
}

func closeQuietly(c io.Closer) {
	if err := c.Close(); err != nil {
		return // read-only handles: nothing was written that could be lost
	}
}
