// encfile_test.go: saving, reading and removing secrets in the file
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package encfile

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

var _ secrets.Store = (*File)(nil)

// fast is the fewest iterations allowed: tests derive many keys.
const fast = minIterations

func pass(p string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return p, nil }
}

func openStore(t *testing.T, path, passphrase string) *File {
	t.Helper()
	f, err := New(context.Background(), Config{Path: path, Passphrase: pass(passphrase), Iterations: fast})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestSetLookupDeleteAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	ctx := context.Background()
	f := openStore(t, path, "frase segreta")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the file was made before the first Set")
	}
	values := map[string]string{"OPENAI_API_KEY": "sk-1", "EMPTY": "", "UNICODE": "àèì € 日本\n\"x\""}
	for name, value := range values {
		if err := f.Set(ctx, name, value); err != nil {
			t.Fatal(err)
		}
	}
	again := openStore(t, path, "frase segreta") // another process, later
	checkAll(t, again, values)
	if err := again.Delete(ctx, "OPENAI_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := f.Lookup(ctx, "OPENAI_API_KEY"); found {
		t.Fatal("the other store's delete is not seen")
	}
}

func checkAll(t *testing.T, f *File, values map[string]string) {
	t.Helper()
	for name, value := range values {
		if got, found, err := f.Lookup(context.Background(), name); err != nil || !found || got != value {
			t.Fatalf("%s: %q %v %v", name, got, found, err)
		}
	}
}

func TestDeletingWhatIsNotThereLeavesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	f := openStore(t, path, "p")
	if err := f.Set(context.Background(), "A", "1"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := f.Delete(context.Background(), "NOPE"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("the file was rewritten for nothing")
	}
}

// Two processes writing at once keep each other's secrets (eos-legacy
// was single-process: the last writer's map won).
func TestConcurrentWritersKeepBoth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	a, b := openStore(t, path, "p"), openStore(t, path, "p")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			if err := s.Set(context.Background(), "K"+string(rune('A'+i)), "v"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := range 20 {
		if _, found, err := a.Lookup(context.Background(), "K"+string(rune('A'+i))); err != nil || !found {
			t.Fatalf("K%c lost: %v", rune('A'+i), err)
		}
	}
}

// A file made with fewer iterations than configured is rewritten with
// them, under a new salt.
func TestIterationsGoUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	low := openStore(t, path, "p")
	if err := low.Set(context.Background(), "A", "1"); err != nil {
		t.Fatal(err)
	}
	high, err := New(context.Background(), Config{Path: path, Passphrase: pass("p"), Iterations: 2 * fast})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = high.Close() }()
	if err := high.Set(context.Background(), "B", "2"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	h, _, _, err := parse(data)
	if err != nil || h.iters != 2*fast {
		t.Fatalf("iterations %d %v", h.iters, err)
	}
	if v, _, err := low.Lookup(context.Background(), "A"); err != nil || v != "1" {
		t.Fatalf("%q %v", v, err)
	}
}

func TestConfigChecked(t *testing.T) {
	dir := t.TempDir()
	for _, cfg := range []Config{
		{Path: filepath.Join(dir, "s"), Iterations: fast},                                   // no passphrase
		{Passphrase: pass("p"), Iterations: fast},                                           // no path
		{Path: filepath.Join(dir, "s"), Passphrase: pass("p"), Iterations: 1000},            // too few
		{Path: filepath.Join(dir, "missing", "s"), Passphrase: pass("p"), Iterations: fast}, // no directory
	} {
		if _, err := New(context.Background(), cfg); !errors.HasCode(err, secrets.ErrSource) {
			t.Errorf("%+v: %v", cfg, err)
		}
	}
}
