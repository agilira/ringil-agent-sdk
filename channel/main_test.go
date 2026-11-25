// main_test.go: the tests keep the program's places in a temporary home
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"testing"

	"github.com/agilira/ringil-agent-sdk/internal/testhome"
)

func TestMain(m *testing.M) { testhome.Main(m) }

// ephemeral is cfg for a test: its sessions in memory unless it names a
// file, so tests never share the default one.
func ephemeral(cfg Config) Config {
	cfg.Ephemeral = true
	return cfg
}
