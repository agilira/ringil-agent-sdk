// hidden.go: listing what the exclusion policy hides inside a directory
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/agilira/go-errors"
)

// MaxHiddenWalk bounds the entries Hidden visits: far above a developer's
// home (two million entries, 6 s, measured 21/11; one million refused it
// and with it every sandboxed command).
const MaxHiddenWalk = 1 << 24

// Hidden returns the paths under dir, relative and slash-separated, that
// the exclusion patterns hide (DefaultExclude when patterns is nil). A
// hidden directory is listed once, not descended into. Hosts use it to
// give other tools the same view as the file tools: a sandbox mounting dir
// masks these paths, so a script cannot read what read_file refuses.
// A tree larger than MaxHiddenWalk is an error: an incomplete list would
// leave secrets visible.
func Hidden(ctx context.Context, dir string, patterns []string) ([]string, error) {
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
	w := &hiddenWalk{ctx: ctx, pol: pol, real: real, dir: dir}
	if pol.excluded(real) {
		return w.everything()
	}
	err = filepath.WalkDir(real, w.visit)
	return w.hidden, err
}

// everything hides all of dir's entries: dir itself is excluded (a
// start directory inside ~/.ssh).
func (w *hiddenWalk) everything() ([]string, error) {
	entries, err := os.ReadDir(w.real)
	if err != nil {
		return nil, errors.Wrap(err, ErrConfig, "cannot read "+w.dir)
	}
	for _, e := range entries {
		w.hidden = append(w.hidden, e.Name())
	}
	return w.hidden, nil
}

type hiddenWalk struct {
	ctx       context.Context
	pol       policy
	real, dir string
	hidden    []string
	seen      int
}

func (w *hiddenWalk) visit(p string, d fs.DirEntry, err error) error {
	w.seen++
	switch {
	case w.seen&1023 == 0 && w.ctx.Err() != nil:
		return w.ctx.Err()
	case w.seen > MaxHiddenWalk:
		return errors.New(ErrConfig, "too many files under "+w.dir+" to check for secrets")
	case err != nil:
		return nil // unreadable here is unreadable in the sandbox too
	case p == w.real || !w.pol.excludedLeaf(p):
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

// Anchored returns the paths of dir, relative and slash-separated, that the
// exclusion patterns (DefaultExclude when nil) hide at dir's own level, as
// a home's secrets lie: its entries a single-name pattern matches (.ssh,
// .netrc, .env, id_rsa) and what each multi-name pattern names under it
// (.config/gh, .local/share/keyrings). Nothing is walked: it costs one
// directory read whatever dir holds, where Hidden walks it all (a home of
// two million entries took seconds). The sandboxed commands use it for the
// directories they only read, as other agents' sandboxes do; the working
// directory, which they write, gets Hidden.
func Anchored(dir string, patterns []string) ([]string, error) {
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
	if pol.excluded(real) {
		w := &hiddenWalk{real: real, dir: dir}
		return w.everything()
	}
	top, err := topHidden(pol, real, dir)
	if err != nil {
		return nil, err
	}
	return append(top, namedHidden(patterns, real, top)...), nil
}

// topHidden is dir's own entries a pattern hides.
func topHidden(pol policy, real, dir string) ([]string, error) {
	entries, err := os.ReadDir(real)
	if err != nil {
		return nil, errors.Wrap(err, ErrConfig, "cannot read "+dir)
	}
	var out []string
	for _, e := range entries {
		if pol.excludedLeaf(filepath.Join(real, e.Name())) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// namedHidden is what each multi-name pattern names under real, but under
// none of top (hidden whole already). Names are matched as the policy does,
// ignoring case (macOS: ~/.cargo/Credentials.toml), one directory read per
// level the pattern descends.
func namedHidden(patterns []string, real string, top []string) []string {
	seen := map[string]bool{}
	for _, t := range top {
		seen[t] = true
	}
	var out []string
	for _, raw := range patterns {
		parts := strings.Split(fold(strings.Trim(raw, "/")), "/")
		if len(parts) < 2 || seen[parts[0]] {
			continue
		}
		for _, rel := range matchNames(real, "", parts) {
			if !seen[rel] && !seen[strings.SplitN(rel, "/", 2)[0]] {
				seen[rel] = true
				out = append(out, rel)
			}
		}
	}
	return out
}

// matchNames is the paths under real/rel whose names match parts, one
// component each.
func matchNames(real, rel string, parts []string) []string {
	if len(parts) == 0 {
		return []string{rel}
	}
	entries, err := os.ReadDir(filepath.Join(real, filepath.FromSlash(rel)))
	if err != nil {
		return nil
	}
	pat := compilePart(parts[0])
	var out []string
	for _, e := range entries {
		if !pat.match(fold(e.Name())) {
			continue
		}
		next := path.Join(rel, e.Name())
		if len(parts) > 1 && !e.IsDir() {
			continue
		}
		out = append(out, matchNames(real, next, parts[1:])...)
	}
	return out
}
