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
	return info, publish(ctx, src, root, info)
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
	return root, nil
}

// publish copies into a staging directory and renames it into place, so
// the catalog only ever shows complete skills.
func publish(ctx context.Context, src, root *os.Root, info Info) error {
	if _, err := root.Lstat(info.Name); err == nil {
		return errors.New(ErrExists, "skill "+info.Name+" is already installed; remove it first")
	}
	stage := stagePrefix + strings.ToLower(rand.Text()[:12])
	if err := root.Mkdir(stage, 0o700); err != nil {
		return errors.Wrap(err, ErrIO, "cannot stage the skill")
	}
	err := stageSkill(ctx, src, root, stage, info)
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

func stageSkill(ctx context.Context, src, root *os.Root, stage string, info Info) error {
	dst, err := root.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer closeQuietly(dst)
	m := manifest{Version: 1, Info: info, Files: map[string]string{}}
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
