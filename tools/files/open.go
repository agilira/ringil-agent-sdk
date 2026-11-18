// open.go: opening a path and checking where it really is
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// opened is a file or directory whose real location has been checked.
type opened struct {
	f    *os.File
	real string
	info os.FileInfo
}

func (o *opened) close() error {
	if err := o.f.Close(); err != nil {
		return errors.Wrap(err, ErrIO, "cannot close")
	}
	return nil
}

// open opens abs and verifies, on the open descriptor, that it is inside a
// grant, not excluded, and a regular file or a directory. Opening is
// non-blocking, so a FIFO cannot hang the agent.
func (w *workspace) open(abs string) (*opened, error) {
	grant, ok := w.grantOf(abs)
	if !ok {
		return nil, w.outside()
	}
	f, err := openIn(grant, abs)
	if err != nil {
		return nil, err
	}
	o, err := w.verify(f)
	if err != nil {
		if cerr := f.Close(); cerr != nil {
			return nil, errors.Wrap(cerr, ErrIO, "cannot close a refused file")
		}
		return nil, err
	}
	return o, nil
}

func openIn(grant, abs string) (*os.File, error) {
	rel, err := filepath.Rel(grant, abs)
	if err != nil {
		return nil, agent.ToolError(ErrOutside, "That path is outside the granted directories.")
	}
	root, err := os.OpenRoot(grant)
	if err != nil {
		return nil, agent.ToolError(ErrIO, "The granted directory cannot be opened.")
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|nonBlock, 0)
	if cerr := root.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return nil, statError(err)
	}
	return f, nil
}

// verify is the security check: everything is decided on where the open
// descriptor really points, not on the path that led there.
func (w *workspace) verify(f *os.File) (*opened, error) {
	real, err := fdPath(f)
	if err != nil {
		return nil, agent.ToolError(ErrIO, "The file's location cannot be verified.")
	}
	if _, ok := w.grantOf(real); !ok {
		return nil, agent.ToolError(ErrOutside, "That path leads outside the granted directories.")
	}
	if w.policy.excluded(real) {
		return nil, excludedError()
	}
	info, err := f.Stat()
	if err != nil {
		return nil, statError(err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, agent.ToolError(ErrInput, "That is not a regular file or a directory.")
	}
	return &opened{f: f, real: real, info: info}, nil
}

// statError turns a filesystem error into what the model may see.
func statError(err error) error {
	if os.IsNotExist(err) {
		return agent.ToolError(ErrNotFound, "No such file or directory.")
	}
	if strings.Contains(err.Error(), "escapes from parent") || strings.Contains(err.Error(), "path escapes") {
		return agent.ToolError(ErrOutside, "That path leads outside the granted directories.")
	}
	return errors.Wrap(err, ErrIO, "cannot access the path")
}

// quoteName shows a name as is unless it holds control characters, which
// could forge extra lines or escape sequences in tool output; such names
// are quoted.
func quoteName(name string) string {
	for _, r := range name {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return strconv.Quote(name)
		}
	}
	return name
}
