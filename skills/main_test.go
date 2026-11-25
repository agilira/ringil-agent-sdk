// main_test.go: the tests keep the program's places in a temporary home
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"path/filepath"
	"testing"

	"github.com/agilira/ringil-agent-sdk/dirs"

	"github.com/agilira/ringil-agent-sdk/internal/testhome"
)

func TestMain(m *testing.M) { testhome.Main(m) }

// Left alone, the scripts' files are kept in the program's outputs, the
// agent's own places are never mounted, and Output has a quota.
func TestOptionsDefaults(t *testing.T) {
	d, err := dirs.Program()
	if err != nil {
		t.Fatal(err)
	}
	o := Options{}.withDefaults()
	if o.Output != filepath.Join(d.Data, "outputs") || len(o.Protected) != 2 || o.OutputQuota != defaultQuota {
		t.Fatalf("%+v", o)
	}
	off := Options{NoOutput: true, NoScripts: true, OutputQuota: -1, Protected: []string{}}.withDefaults()
	if off.Output != "" || off.Sandbox != nil || off.OutputQuota != 0 || len(off.Protected) != 0 {
		t.Fatalf("%+v", off)
	}
}
