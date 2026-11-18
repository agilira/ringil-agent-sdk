// fdpath_other.go: platforms without a way to ask an open file its path
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build !linux && !darwin && !windows

package files

import (
	"errors"
	"os"
)

// fdPath is unsupported here: the tools refuse to read rather than trust
// a path that links or races may have changed.
func fdPath(*os.File) (string, error) {
	return "", errors.New("cannot verify an open file's path on this platform")
}
