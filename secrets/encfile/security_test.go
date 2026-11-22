// security_test.go: what someone with the file, or the disk, could do
//
// THREAT MODEL:
//   - A thief of the file: it holds no name or value in clear, and each
//     guess of the passphrase costs PBKDF2's iterations; the file cannot
//     lower them (a header asking for fewer than the minimum is refused).
//   - Someone who changes the file (a byte, the header, a swap with
//     another file of theirs): refused as a wrong passphrase, never read
//     as "absent" (a chain would fall through to another source).
//   - Another user or a link on the disk: the directory must be private,
//     the file is not followed through a link.
//   - Errors never carry a value or the passphrase.
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package encfile

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

func written(t *testing.T) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.enc")
	f := openStore(t, path, "frase segreta")
	if err := f.Set(context.Background(), "OPENAI_API_KEY", "sk-live-VALUE"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, data
}

func TestSecurityNothingInClear(t *testing.T) {
	_, data := written(t)
	for _, s := range []string{"OPENAI_API_KEY", "sk-live-VALUE", "frase"} {
		if bytes.Contains(data, []byte(s)) {
			t.Fatalf("%q is in the file", s)
		}
	}
}

func TestSecurityWrongPassphrase(t *testing.T) {
	path, _ := written(t)
	_, err := New(context.Background(), Config{Path: path, Passphrase: pass("sbagliata"), Iterations: fast})
	if !errors.HasCode(err, ErrDecrypt) || strings.Contains(err.Error(), "sbagliata") {
		t.Fatalf("%v", err)
	}
}

// Every byte changed, header or body, is refused: never "absent".
func TestSecurityAnyChangeIsRefused(t *testing.T) {
	path, data := written(t)
	f := openStore(t, path, "frase segreta")
	for i := range data {
		if i == 4 || (i >= 5 && i < 9) {
			continue // version and iterations: refused by the format check, below
		}
		changed := append([]byte(nil), data...)
		changed[i] ^= 0x01
		if err := os.WriteFile(path, changed, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, found, err := f.Lookup(context.Background(), "OPENAI_API_KEY"); err == nil || found {
			t.Fatalf("byte %d changed: found %v, %v", i, found, err)
		}
	}
	badHeaders(t, f, path, data)
}

// badHeaders: iterations out of bounds, or a truncated file, are format
// errors.
func badHeaders(t *testing.T, f *File, path string, data []byte) {
	t.Helper()
	for _, iters := range []uint32{1, minIterations - 1, maxIterations + 1} {
		changed := append([]byte(nil), data...)
		binary.BigEndian.PutUint32(changed[5:9], iters)
		_ = os.WriteFile(path, changed, 0o600)
		if _, _, err := f.Lookup(context.Background(), "OPENAI_API_KEY"); !errors.HasCode(err, ErrFile) {
			t.Fatalf("%d iterations: %v", iters, err)
		}
	}
	_ = os.WriteFile(path, data[:headerLen+3], 0o600) // truncated
	if _, _, err := f.Lookup(context.Background(), "OPENAI_API_KEY"); err == nil {
		t.Fatal("a truncated file read")
	}
}

// A file of the same format made with another passphrase, swapped in,
// is refused: the key is not the store's.
func TestSecurityASwappedFile(t *testing.T) {
	path, _ := written(t)
	f := openStore(t, path, "frase segreta")
	other := filepath.Join(t.TempDir(), "other.enc")
	o := openStore(t, other, "altra")
	if err := o.Set(context.Background(), "OPENAI_API_KEY", "sk-attacker"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(other)
	_ = os.WriteFile(path, data, 0o600)
	if v, _, err := f.Lookup(context.Background(), "OPENAI_API_KEY"); !errors.HasCode(err, ErrDecrypt) || v != "" {
		t.Fatalf("%q %v", v, err)
	}
}

func TestSecurityPrivateDirectoryAndNoLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissions and links differ on Windows")
	}
	open := t.TempDir()
	if err := os.Chmod(open, 0o777); err != nil { // #nosec G302 -- the test makes it open on purpose
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Config{Path: filepath.Join(open, "s"), Passphrase: pass("p"), Iterations: fast}); !errors.HasCode(err, secrets.ErrSource) {
		t.Fatalf("an open directory: %v", err)
	}
	path, _ := written(t)
	link := filepath.Join(filepath.Dir(path), "link.enc")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Config{Path: link, Passphrase: pass("frase segreta"), Iterations: fast}); err == nil {
		t.Fatal("the file was read through a link")
	}
}

func TestSecurityNamesAndValues(t *testing.T) {
	f := openStore(t, filepath.Join(t.TempDir(), "s.enc"), "p")
	ctx := context.Background()
	for _, name := range []string{"", "1A", "A-B", "../A"} {
		if err := f.Set(ctx, name, "v"); !errors.HasCode(err, secrets.ErrName) {
			t.Errorf("%q: %v", name, err)
		}
	}
	if err := f.Set(ctx, "A", "a\x00b"); !errors.HasCode(err, secrets.ErrValue) {
		t.Errorf("NUL: %v", err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(magic + "\x01\x00\x00\x27\x10" + strings.Repeat("s", saltLen+nonceLen+16)))
	f.Add([]byte("RGSF"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		h, nonce, body, err := parse(data)
		if err != nil {
			return
		}
		if h.iters < minIterations || h.iters > maxIterations || len(nonce) != nonceLen || len(h.salt) != saltLen || len(body) < 16 {
			t.Fatalf("accepted %+v", h)
		}
	})
}

// Audit 21/11: values that JSON escapes (a control character takes six
// bytes) made a file no call could read again. A write past the bound is
// refused, and the store goes on.
func TestSecurityTheFileStaysReadable(t *testing.T) {
	saved := maxFile
	maxFile = 1 << 20 // the same bound, reached in a few writes
	t.Cleanup(func() { maxFile = saved })
	f := openStore(t, filepath.Join(t.TempDir(), "s.enc"), "p")
	ctx := context.Background()
	big := strings.Repeat("\x01", 64<<10-1)
	var refused error
	for i := range 300 {
		if refused = f.Set(ctx, "K"+strconv.Itoa(i), big); refused != nil {
			break
		}
	}
	if !errors.HasCode(refused, secrets.ErrValue) {
		t.Fatalf("no bound: %v", refused)
	}
	if _, found, err := f.Lookup(ctx, "K0"); err != nil || !found {
		t.Fatalf("the store broke: %v", err)
	}
	if err := f.Delete(ctx, "K0"); err != nil {
		t.Fatal(err)
	}
}

// Audit 21/11: a file deleted after it was read became "no secrets", and
// a chain fell through to another source.
func TestSecurityAFileThatGoesIsAnError(t *testing.T) {
	path, _ := written(t)
	f := openStore(t, path, "frase segreta")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.Lookup(context.Background(), "OPENAI_API_KEY"); !errors.HasCode(err, ErrFile) || found {
		t.Fatalf("%v %v", found, err)
	}
}

func TestSecurityOddPaths(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(context.Background(), Config{Path: filepath.Join(dir, "s") + string(filepath.Separator), Passphrase: pass("p"), Iterations: fast}); !errors.HasCode(err, secrets.ErrSource) {
		t.Errorf("a trailing separator: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	real := t.TempDir()
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Config{Path: filepath.Join(link, "s"), Passphrase: pass("p"), Iterations: fast}); !errors.HasCode(err, secrets.ErrSource) {
		t.Errorf("a linked directory: %v", err)
	}
}

func TestSecurityWrongPassphraseIsNotCached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")
	current := "wrong"
	f, err := New(context.Background(), Config{
		Path:       path,
		Passphrase: func(context.Context) (string, error) { return current, nil },
		Iterations: fast,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	other := openStore(t, path, "right")
	if err := other.Set(context.Background(), "OPENAI_API_KEY", "sk-ok"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Lookup(context.Background(), "OPENAI_API_KEY"); !errors.HasCode(err, ErrDecrypt) {
		t.Fatalf("first lookup: %v", err)
	}
	current = "right"
	if v, found, err := f.Lookup(context.Background(), "OPENAI_API_KEY"); err != nil || !found || v != "sk-ok" {
		t.Fatalf("second lookup: %q %v %v", v, found, err)
	}
}
