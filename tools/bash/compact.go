// compact.go: fewer hidden paths, by hiding the directories that hold many
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agilira/ringil-agent-sdk/sandbox"
)

// fit brings the hidden paths of mounts within what the sandbox takes
// (sandbox.MaxHidden): in the mount that hides most, the directory holding
// most hidden paths is hidden in their place, and so on, until they fit.
// More is hidden, never less. A home holds hundreds of test keys and
// certificates in module caches and SDKs (1296 measured 21/11), which
// would otherwise keep every command from starting.
func fit(mounts []sandbox.Mount) {
	for over := hiddenCount(mounts) - sandbox.MaxHidden; over > 0; over = hiddenCount(mounts) - sandbox.MaxHidden {
		i := mostHidden(mounts)
		before := len(mounts[i].Hide)
		mounts[i].Hide = compact(mounts[i].Hide, before-over, func(dir string) bool {
			return holdsMount(path.Join(filepath.ToSlash(target(mounts[i])), dir), mounts, i)
		})
		if len(mounts[i].Hide) >= before {
			return // nothing more to fold: the sandbox says it does not fit
		}
	}
}

func hiddenCount(mounts []sandbox.Mount) int {
	n := 0
	for _, m := range mounts {
		n += len(m.Hide) + len(m.ReadOnly)
	}
	return n
}

func mostHidden(mounts []sandbox.Mount) int {
	most := 0
	for i, m := range mounts {
		if len(m.Hide) > len(mounts[most].Hide) {
			most = i
		}
	}
	return most
}

// holdsMount: dir (inside the sandbox) is, or holds, another mount's
// target, which hiding it would cover.
func holdsMount(dir string, mounts []sandbox.Mount, self int) bool {
	for j, m := range mounts {
		t := filepath.ToSlash(target(m))
		if j != self && (t == dir || strings.HasPrefix(t, dir+"/")) {
			return true
		}
	}
	return false
}

// compact folds hidden paths (relative, slash-separated) into the
// directories that hold them, deepest first, until at most budget remain
// or nothing folds; blocked directories are never hidden.
func compact(hide []string, budget int, blocked func(dir string) bool) []string {
	set := map[string]bool{}
	for _, h := range hide {
		set[h] = true
	}
	for len(set) > budget {
		dir := busiest(set, blocked)
		if dir == "" {
			break
		}
		for h := range set {
			if strings.HasPrefix(h, dir+"/") {
				delete(set, h)
			}
		}
		set[dir] = true
	}
	out := make([]string, 0, len(set))
	for _, h := range hide {
		if set[h] {
			out = append(out, h)
			delete(set, h)
		}
	}
	folded := make([]string, 0, len(set))
	for h := range set { // the directories folded into
		folded = append(folded, h)
	}
	sort.Strings(folded)
	return append(out, folded...)
}

// busiest is the directory to fold into: the deepest holding at least
// two hidden paths (folding gains, and hides least beyond them), the one
// holding more on a tie; "" when none.
func busiest(set map[string]bool, blocked func(string) bool) string {
	count := map[string]int{}
	for h := range set {
		for d := path.Dir(h); d != "."; d = path.Dir(d) {
			count[d]++
		}
	}
	best := ""
	for d, n := range count {
		if n >= 2 && !blocked(d) && (best == "" || before(d, n, best, count[best])) {
			best = d
		}
	}
	return best
}

// before: d (holding n) is a better fold than best (holding m): deeper,
// then holding more, then first by name.
func before(d string, n int, best string, m int) bool {
	dd, bd := strings.Count(d, "/"), strings.Count(best, "/")
	if dd != bd {
		return dd > bd
	}
	if n != m {
		return n > m
	}
	return d < best
}
