// atomicfile_test.go: writes in place and in subdirectories
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package atomicfile

import (
	"os"
	"testing"
)

// Audit 18/11: the temporary name of "dir/x" was ".dir/x-...", in a
// directory that does not exist, so nested files could not be written.
func TestWriteInSubdirectory(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Log(err)
		}
	}()
	if err := root.MkdirAll("a/b", 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"top.md", "a/b/deep.md"} {
		writeRead(t, root, name)
	}
	entries, err := os.ReadDir(root.Name() + "/a/b")
	if err != nil || len(entries) != 1 {
		t.Fatalf("leftovers %v %v", entries, err)
	}
}

func writeRead(t *testing.T, root *os.Root, name string) {
	t.Helper()
	if err := Write(root, name, []byte("x")); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if got, err := Read(root, name, 10); err != nil || string(got) != "x" {
		t.Fatalf("%s: %q %v", name, got, err)
	}
}
