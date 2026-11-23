// security_test.go: the package's boundaries under arbitrary input
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Whatever the names and values, Copy writes only valid names, reads each
// back the same, and its errors never carry a value.
func FuzzCopy(f *testing.F) {
	f.Add("A_KEY", "sk-1", "B", "x\ny", false)
	f.Add("bad name", "v", "C", "", true)
	f.Add("A", "", "A", "same", false)
	f.Fuzz(func(t *testing.T, n1, v1, n2, v2 string, lie bool) {
		// The values carry a mark no name and no message has: an error
		// with the mark has a value in it.
		const mark = "\x01value:"
		from := Map{n1: mark + v1, n2: mark + v2}
		to := newMem()
		if lie {
			to.mangle = func(v string) string { return v + "!" }
		}
		done, err := Copy(context.Background(), from, to, []string{n1, n2})
		if err != nil && strings.Contains(err.Error(), mark) {
			t.Fatalf("a value in the error: %q", err)
		}
		for _, name := range done.Names {
			if ValidateName(name) != nil || to.Map[name] != from[name] {
				t.Fatalf("copied %q wrong", name)
			}
		}
		if lie && len(done.Names) > 0 {
			t.Fatal("a store that gives back something else was trusted")
		}
	})
}

// Any value written to an env file reads back the same, or the file is
// not left behind.
func FuzzWriteEnvFile(f *testing.F) {
	for _, v := range []string{"", "a b", "x\ny", `"\`, "#c", "'q'", "\x00\xff", " lead", "trail ", "a #b"} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, value string) {
		path := t.TempDir() + "/f.env"
		err := WriteEnvFile(path, Map{"K": value})
		if err != nil {
			if _, statErr := os.Stat(path); statErr == nil {
				t.Fatalf("a failed write left the file: %v", err)
			}
			return
		}
		res, err := EnvFile(path)
		if err != nil || res.(Map)["K"] != value {
			t.Fatalf("%q came back as %q (%v)", value, res, err)
		}
	})
}
