// main_test.go: the tests keep the program's places in a temporary home
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"testing"

	"github.com/agilira/ringil-agent-sdk/dirs"
	"github.com/agilira/ringil-agent-sdk/internal/testhome"
)

func TestMain(m *testing.M) { testhome.Main(m) }

// Left alone, the agent's own places are never granted to a command.
func TestNeverDefaultsToTheProgramsOwnPlaces(t *testing.T) {
	d, err := dirs.Program()
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	defaults(&cfg)
	if len(cfg.Never) != 2 || cfg.Never[0] != d.Config || cfg.Never[1] != d.State {
		t.Fatalf("never %v", cfg.Never)
	}
	off := Config{NoNever: true}
	defaults(&off)
	if len(off.Never) != 0 {
		t.Fatalf("NoNever kept %v", off.Never)
	}
}
