// read.go: reading a data file that someone may have replaced
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package atomicfile

import (
	"errors"
	"io"
	"os"

	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

// ErrNotRegular: the name is not a regular file of sane size.
var ErrNotRegular = errors.New("not a regular file of sane size")

// Read reads name in root only if it is a regular file of at most max
// bytes. It opens without blocking and follows no link at the name, so a
// FIFO or a link planted there cannot hang or redirect the read.
func Read(root *os.Root, name string, max int64) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|nonBlock|lockfile.NoFollow, 0)
	if err != nil {
		return nil, err
	}
	data, err := readRegular(f, max)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return data, err
}

func readRegular(f *os.File, max int64) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return nil, ErrNotRegular
	}
	return io.ReadAll(io.LimitReader(f, max))
}
