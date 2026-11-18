// shortname_windows.go: 8.3 short names
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"regexp"

	agent "github.com/agilira/ringil-agent-sdk"
)

var short83 = regexp.MustCompile(`~[0-9]`)

// shortName refuses names that may be 8.3 short aliases (CLAUDE~1.MD):
// the checks see the alias, the write would reach the real file.
func shortName(name string) error {
	if short83.MatchString(name) {
		return agent.ToolError(ErrInput, "Use the file's full name, not a short ~1 alias.")
	}
	return nil
}
