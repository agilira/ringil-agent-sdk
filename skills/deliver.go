// deliver.go: handing over the files a script created
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// deliverFiles copies the regular files under work into dest, keeping
// their relative paths, and returns the host paths written. It never
// replaces anything: a taken name becomes "name-1.ext", "name-2.ext"...
// Links and special files the script made are left behind. Every write
// goes through an os.Root on dest, so no path can leave it.
func deliverFiles(work, dest string) ([]string, error) {
	src, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(src)
	dst, err := os.OpenRoot(dest)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(dst)
	var saved []string
	err = fs.WalkDir(src.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		name, err := deliverOne(src, dst, p)
		if err != nil {
			return err
		}
		saved = append(saved, filepath.Join(dest, filepath.FromSlash(name)))
		return nil
	})
	return saved, err
}

func deliverOne(src, dst *os.Root, p string) (string, error) {
	in, err := src.OpenFile(p, os.O_RDONLY|nonBlock, 0)
	if err != nil {
		return "", err
	}
	defer closeQuietly(in)
	if dir := path.Dir(p); dir != "." {
		if err := dst.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	out, name, err := createFree(dst, p)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return name, err
}

// createFree creates p, or the first free numbered variant of it.
func createFree(dst *os.Root, p string) (*os.File, string, error) {
	ext := path.Ext(p)
	base := strings.TrimSuffix(p, ext)
	name := p
	for i := 1; ; i++ {
		f, err := dst.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil || !os.IsExist(err) || i > 999 {
			return f, name, err
		}
		name = base + "-" + strconv.Itoa(i) + ext
	}
}
