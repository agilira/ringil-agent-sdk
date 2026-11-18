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

func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
