// sync_unix.go: syncing a directory after a rename
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package local

import (
	"os"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// syncDir makes the rename itself durable.
func syncDir(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot open the memory directory")
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot sync the memory directory")
	}
	return nil
}
