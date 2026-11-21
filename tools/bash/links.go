// links.go: hiding what a hidden symbolic link points to
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agilira/ringil-agent-sdk/sandbox"
)

// hideLinkTargets hides, in the mount that grants it, what each hidden
// symbolic link points to: a link stays visible in the sandbox and
// resolves there, so ~/.ssh -> dotfiles/ssh (stow and the like) would
// show the keys through it (audit 21/11). A target no mount grants is not
// in the sandbox at all.
func hideLinkTargets(mounts []sandbox.Mount) {
	sources := make([]string, len(mounts))
	for i, m := range mounts {
		sources[i] = m.Source
		if real, err := filepath.EvalSymlinks(m.Source); err == nil {
			sources[i] = real
		}
	}
	for i := range mounts {
		for _, h := range mounts[i].Hide {
			target, ok := linkTarget(filepath.Join(mounts[i].Source, filepath.FromSlash(h)))
			if !ok {
				continue
			}
			if j, rel, ok := grantedAt(sources, target); ok && !covers(mounts[j].Hide, rel) {
				mounts[j].Hide = append(mounts[j].Hide, rel)
			}
		}
	}
}

// linkTarget is where p, a symbolic link, really points; not ok for what
// is not a link, or points to nothing.
func linkTarget(p string) (string, bool) {
	if info, err := os.Lstat(p); err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	target, err := filepath.EvalSymlinks(p)
	return target, err == nil
}

// grantedAt is the mount (the deepest) whose source holds p, and p in it.
func grantedAt(sources []string, p string) (int, string, bool) {
	best, rel := -1, ""
	for i, s := range sources {
		r, err := filepath.Rel(s, p)
		if err != nil || r == "." || !filepath.IsLocal(r) {
			continue
		}
		if best < 0 || len(s) > len(sources[best]) {
			best, rel = i, filepath.ToSlash(r)
		}
	}
	return best, rel, best >= 0
}

// covers: rel is hidden, or under a hidden path.
func covers(hide []string, rel string) bool {
	return slices.ContainsFunc(hide, func(h string) bool { return rel == h || strings.HasPrefix(rel, h+"/") })
}
