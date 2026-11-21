// paths.go: resolving and comparing host paths
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package sandbox

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/agilira/go-errors"
)

// resolve returns p with every existing component's symlinks resolved and
// the missing tail appended.
func resolve(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", errors.New(ErrConfig, "protected paths must be absolute: "+p)
	}
	p = filepath.Clean(p)
	var tail []string
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{real}, tail...)...), nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p, nil
		}
		tail = append([]string{filepath.Base(p)}, tail...)
		p = parent
	}
}

// masked: the protected path prot, inside the mount m whose source is
// really at mountReal, is one of its hidden paths or lies under one, so the
// program never sees it. In a writable mount it must also exist now (on
// Linux a hidden path that does not exist is not covered, and the program
// could create it on the host).
func masked(m Mount, mountReal, prot string) bool {
	rel, err := filepath.Rel(mountReal, prot)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return false
	}
	if _, err := os.Lstat(prot); m.Writable && err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	for _, h := range m.Hide {
		if rel == h || strings.HasPrefix(rel, h+"/") {
			return true
		}
	}
	return false
}

func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
