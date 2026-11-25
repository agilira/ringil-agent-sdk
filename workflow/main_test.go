// main_test.go: the tests keep the program's places in a temporary home
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package workflow

import (
	"path/filepath"
	"testing"

	"github.com/agilira/ringil-agent-sdk/dirs"
	"github.com/agilira/ringil-agent-sdk/internal/testhome"
)

func TestMain(m *testing.M) { testhome.Main(m) }

// Left alone, a runner keeps its runs in a journal in the program's
// state: they can be resumed after a restart.
func TestJournalDefaultsToAFile(t *testing.T) {
	d, err := dirs.Program()
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{}
	r.defaultJournal()
	j, ok := r.Journal.(*FileJournal)
	if !ok || j.dir != filepath.Join(d.State, "workflows") {
		t.Fatalf("journal %#v", r.Journal)
	}
	none := &Runner{NoJournal: true}
	none.defaultJournal()
	if none.Journal != nil {
		t.Fatal("NoJournal got a journal")
	}
}
