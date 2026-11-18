// hidden.go: listing what the exclusion policy hides inside a directory
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"io/fs"
	"path/filepath"

	"github.com/agilira/go-errors"
)

// MaxHiddenWalk bounds the entries Hidden visits.
const MaxHiddenWalk = 1 << 20

// Hidden returns the paths under dir, relative and slash-separated, that
// the exclusion patterns hide (DefaultExclude when patterns is nil). A
// hidden directory is listed once, not descended into. Hosts use it to
// give other tools the same view as the file tools: a sandbox mounting dir
// masks these paths, so a script cannot read what read_file refuses.
// A tree larger than MaxHiddenWalk is an error: an incomplete list would
// leave secrets visible.
func Hidden(dir string, patterns []string) ([]string, error) {
	if patterns == nil {
		patterns = DefaultExclude
	}
	pol, err := newPolicy(patterns)
	if err != nil {
		return nil, errors.Wrap(err, ErrConfig, "bad exclude pattern")
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrConfig, "cannot resolve "+dir)
	}
	w := &hiddenWalk{pol: pol, real: real, dir: dir}
	err = filepath.WalkDir(real, w.visit)
	return w.hidden, err
}

type hiddenWalk struct {
	pol       policy
	real, dir string
	hidden    []string
	seen      int
}

func (w *hiddenWalk) visit(p string, d fs.DirEntry, err error) error {
	w.seen++
	switch {
	case w.seen > MaxHiddenWalk:
		return errors.New(ErrConfig, "too many files under "+w.dir+" to check for secrets")
	case err != nil:
		return nil // unreadable here is unreadable in the sandbox too
	case p == w.real || !w.pol.excluded(p):
		return nil
	}
	rel, err := filepath.Rel(w.real, p)
	if err != nil {
		return err
	}
	w.hidden = append(w.hidden, filepath.ToSlash(rel))
	if d.IsDir() {
		return filepath.SkipDir
	}
	return nil
}
