// security_test.go: scope, quota, integrity, and permission tests for artifacts
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package artifact

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

func TestSecurityScopeNameAndMediaValidation(t *testing.T) {
	s, _ := store(t)
	badScopes := []string{"", ".", "..", "../x", ".hidden", "a/b", `a\b`, "a\x00b", strings.Repeat("s", 101)}
	for _, sc := range badScopes {
		if _, err := s.Put(bg, sc, "ok.txt", "text/plain", "", []byte("x")); !errors.HasCode(err, ErrInput) {
			t.Errorf("scope %q accepted: %v", sc, err)
		}
	}
	badNames := []string{"", "-a", "_a", ".a", "../x", "a/b", strings.Repeat("n", 101)}
	for _, nm := range badNames {
		if _, err := s.Put(bg, "proj", nm, "text/plain", "", []byte("x")); !errors.HasCode(err, ErrInput) {
			t.Errorf("name %q accepted: %v", nm, err)
		}
	}
	for _, mt := range []string{"text/plain; evil", "../x", "TEXT/PLAIN", ""} {
		info, err := s.Put(bg, "proj", "m.txt", mt, "", []byte("x"))
		if err != nil {
			t.Fatalf("put with media %q failed: %v", mt, err)
		}
		if info.MediaType != "application/octet-stream" {
			t.Errorf("media %q normalized to %q, want application/octet-stream", mt, info.MediaType)
		}
	}
}

func TestSecurityQuotasAndScopeIsolation(t *testing.T) {
	s, _ := store(t)
	s.MaxSize = 10
	s.MaxScope = 15
	if _, err := s.Put(bg, "own", "big.txt", "text/plain", "", []byte("01234567890")); !errors.HasCode(err, ErrInput) {
		t.Fatalf("oversize Put accepted: %v", err)
	}
	if _, err := s.Put(bg, "own", "a.txt", "text/plain", "", []byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(bg, "own", "b.txt", "text/plain", "", []byte("12345678")); !errors.HasCode(err, ErrInput) {
		t.Fatalf("scope quota exceeded accepted: %v", err)
	}
	checkScopeIsolation(t, s)
}

func checkScopeIsolation(t *testing.T, s *Local) {
	t.Helper()
	if _, err := s.Put(bg, "secret", "k.txt", "text/plain", "", []byte("top")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(bg, "shared", "bin.dat", "application/octet-stream", "", []byte{0xff, 0xfe, 0x00}); err != nil {
		t.Fatal(err)
	}
	byName := index2(Tools(s, Access{Scope: "own", Read: []string{"shared"}, Publish: false}))
	if _, ok := byName["publish_artifact"]; ok {
		t.Fatal("publish_artifact exposed when Publish=false")
	}
	if _, err := call(t, byName["list_artifacts"], listIn{Scope: "secret"}); err == nil {
		t.Fatal("listed ungranted scope")
	}
	if _, err := call(t, byName["read_artifact"], readIn{Name: "k.txt", Scope: "secret"}); err == nil {
		t.Fatal("read ungranted scope")
	}
	out, err := call(t, byName["read_artifact"], readIn{Name: "bin.dat", Scope: "shared"})
	if err != nil || !strings.Contains(out, "binary") {
		t.Fatalf("binary artifact response: %q %v", out, err)
	}
}

func TestSecurityCorruptIndexMissingBlobAndPermissions(t *testing.T) {
	s, dir := store(t)
	info, err := s.Put(bg, "proj", "a.txt", "text/plain", "", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "proj", blobDir, info.SHA256)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(bg, "proj", "a.txt", 0); !errors.HasCode(err, ErrDamaged) {
		t.Fatalf("missing blob returned %v, want ErrDamaged", err)
	}
	checkCorruptIndex(t, s, filepath.Join(dir, "proj", indexFile))
	checkNonWindowsPerms(t)
}

func checkCorruptIndex(t *testing.T, s *Local, idxPath string) {
	t.Helper()
	for _, bad := range []string{"{corrupt", `{"version":2,"artifacts":{}}`, `{"version":1,"artifacts":null}`} {
		if err := os.WriteFile(idxPath, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.List(bg, "proj"); !errors.HasCode(err, ErrDamaged) {
			t.Errorf("index %s: List returned %v, want ErrDamaged", bad, err)
		}
		if _, _, err := s.Get(bg, "proj", "a.txt", 0); !errors.HasCode(err, ErrDamaged) {
			t.Errorf("index %s: Get returned %v, want ErrDamaged", bad, err)
		}
	}
}

func checkNonWindowsPerms(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	base := t.TempDir()
	pub := filepath.Join(base, "pub")
	if err := os.Mkdir(pub, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pub, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(pub); !errors.HasCode(err, ErrIO) {
		t.Fatalf("NewLocal accepted 0777 dir: %v", err)
	}
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(link); !errors.HasCode(err, ErrIO) {
		t.Fatalf("NewLocal accepted symlink dir: %v", err)
	}
}
