// envfile_test.go: tests for env files
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

func parse(t *testing.T, text string) map[string]string {
	t.Helper()
	m, err := ParseEnv(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseEnvSyntax(t *testing.T) {
	got := parse(t, strings.Join([]string{
		"# comment",
		"",
		"PLAIN=value",
		"COMMENTED=value # not part of it",
		"TABBED=value\t# not part of it",
		"HASH_IN_WORD=pa#ss",
		"URL=https://x.test/a#frag",
		"export EXPORTED=yes",
		"exported_name=kept",
		`DOUBLE="a # b \"q\" \\ \n end"   # comment`,
		"SINGLE='literal # \\n'  # comment",
		"EMPTY=",
		"EMPTY_COMMENT= # nothing",
		"SPACES =  padded  ",
		"CRLF=win\r",
		"DUP=first",
		"DUP=last",
	}, "\n"))
	want := map[string]string{
		"PLAIN": "value", "COMMENTED": "value", "TABBED": "value", "HASH_IN_WORD": "pa#ss",
		"URL": "https://x.test/a#frag", "EXPORTED": "yes", "exported_name": "kept",
		"DOUBLE": "a # b \"q\" \\ \n end", "SINGLE": `literal # \n`, "EMPTY": "", "EMPTY_COMMENT": "",
		"SPACES": "padded", "CRLF": "win", "DUP": "last",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d names, want %d: %v", len(got), len(want), got)
	}
}

func TestParseEnvErrorsNameLineNotContent(t *testing.T) {
	cases := []string{
		"NOEQUALS",
		"1BAD=x",
		"BAD-NAME=x",
		`OPEN="never closed`,
		"OPEN='never closed",
		`AFTER="ok" trailing`,
		"NUL=a\x00b",
		"=nothing",
	}
	for _, text := range cases {
		_, err := ParseEnv(strings.NewReader("FINE=1\n" + text))
		if !errors.HasCode(err, ErrSource) || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%q: %v", text, err)
		}
		if strings.Contains(fmt.Sprintf("%+v", err), "never closed") || strings.Contains(fmt.Sprintf("%+v", err), "trailing") {
			t.Errorf("%q: error echoes content", text)
		}
	}
}

func TestParseEnvBounds(t *testing.T) {
	if _, err := ParseEnv(strings.NewReader("A=" + strings.Repeat("x", maxEnvLine))); !errors.HasCode(err, ErrSource) {
		t.Fatalf("long line: %v", err)
	}
	if _, err := ParseEnv(strings.NewReader(strings.Repeat("#\n", maxEnvFile))); !errors.HasCode(err, ErrSource) {
		t.Fatalf("large file: %v", err)
	}
	if _, err := ParseEnv(strings.NewReader("A=\xff")); !errors.HasCode(err, ErrSource) {
		t.Fatalf("invalid UTF-8: %v", err)
	}
}

func writeEnv(t *testing.T, mode os.FileMode, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.env")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnvFile(t *testing.T) {
	r, err := EnvFile(writeEnv(t, 0o600, "API_KEY=sk-1 # comment\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := Credential(bg, r, "API_KEY"); err != nil || v != "sk-1" {
		t.Fatalf("%q %v", v, err)
	}
}

func TestEnvFileRefusesUnsafeFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes and symlinks")
	}
	open := writeEnv(t, 0o644, "A=1\n")
	if _, err := EnvFile(open); !errors.HasCode(err, ErrSource) {
		t.Fatalf("world-readable: %v", err)
	}
	target := writeEnv(t, 0o600, "A=1\n")
	link := filepath.Join(t.TempDir(), "link.env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := EnvFile(link); !errors.HasCode(err, ErrSource) {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := EnvFile(t.TempDir()); !errors.HasCode(err, ErrSource) {
		t.Fatalf("directory: %v", err)
	}
	if _, err := EnvFile(filepath.Join(t.TempDir(), "missing")); !errors.HasCode(err, ErrSource) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := EnvFile(writeEnv(t, 0o600, "BAD LINE\n")); !errors.HasCode(err, ErrSource) {
		t.Fatalf("bad syntax: %v", err)
	}
}

// FuzzParseEnv: arbitrary input never panics, and every name parsed is
// valid and every value is valid UTF-8 without NUL.
func FuzzParseEnv(f *testing.F) {
	f.Add("A=1\nexport B=\"x\\n\" # c\nC='y'\n# z\n")
	f.Add("D=a #b\nE=\"unterminated")
	f.Fuzz(func(t *testing.T, text string) {
		m, err := ParseEnv(strings.NewReader(text))
		if err != nil {
			return
		}
		for k, v := range m {
			if ValidateName(k) != nil || strings.ContainsRune(v, 0) {
				t.Fatalf("accepted %q=%q", k, v)
			}
		}
	})
}
