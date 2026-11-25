// main_test.go: where a server's sign-in is kept
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"path/filepath"
	"testing"

	"github.com/agilira/ringil-agent-sdk/dirs"
)

// A server's sign-in is kept in the program's state: the user signs in
// once, not at every start.
func TestLoginTokensDefaultToAFile(t *testing.T) {
	d, err := dirs.Program()
	if err != nil {
		t.Fatal(err)
	}
	store, err := defaultTokens("linear")
	if err != nil || store != FileTokens(filepath.Join(d.State, "mcp-tokens", "linear.json")) {
		t.Fatalf("%v %v", store, err)
	}
	if _, err := defaultTokens("../x"); err == nil {
		t.Fatal("a bad name was taken as a file name")
	}
}
