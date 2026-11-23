// copy_test.go: secrets move between stores, and are checked
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

// memStore is a Store in memory; mangle changes what it gives back, and
// failSet makes Set fail.
type memStore struct {
	Map
	mangle  func(string) string
	failSet bool
	sets    int
}

func newMem() *memStore { return &memStore{Map: Map{}} }

func (m *memStore) Lookup(ctx context.Context, name string) (string, bool, error) {
	v, ok, err := m.Map.Lookup(ctx, name)
	if ok && m.mangle != nil {
		v = m.mangle(v)
	}
	return v, ok, err
}

func (m *memStore) Set(_ context.Context, name, value string) error {
	m.sets++
	if m.failSet {
		return errors.New(ErrLookup, "the backend refused")
	}
	m.Map[name] = value
	return nil
}

func (m *memStore) Delete(_ context.Context, name string) error {
	delete(m.Map, name)
	return nil
}

func TestCopyMovesAndChecks(t *testing.T) {
	from := Map{"A_KEY": "a", "B_KEY": "multi\nline", "OTHER": "x"}
	to := newMem()
	got, err := Copy(context.Background(), from, to, []string{"B_KEY", "A_KEY", "GONE", "A_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, Copied{Names: []string{"A_KEY", "B_KEY"}, Missing: []string{"GONE"}}) {
		t.Fatalf("%+v", got)
	}
	if !reflect.DeepEqual(to.Map, Map{"A_KEY": "a", "B_KEY": "multi\nline"}) || len(from) != 3 {
		t.Fatalf("to %v, from %v", to.Map, from)
	}
}

// A store that gives back something else stops the copy, naming the
// secret and never its value; a refused Set too.
func TestCopyRefusesWhatDoesNotReadBack(t *testing.T) {
	to := newMem()
	to.mangle = func(v string) string { return v[:len(v)-1] }
	_, err := Copy(context.Background(), Map{"A_KEY": "sk-secret-value", "B_KEY": "b"}, to, []string{"A_KEY", "B_KEY"})
	if !errors.HasCode(err, ErrCopy) || !strings.Contains(err.Error(), "A_KEY") || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("%v", err)
	}
	if to.sets != 1 {
		t.Fatalf("went on after a failed check: %d writes", to.sets)
	}
	to = newMem()
	to.failSet = true
	if done, err := Copy(context.Background(), Map{"A_KEY": "a"}, to, []string{"A_KEY"}); err == nil || len(done.Names) != 0 {
		t.Fatalf("%+v %v", done, err)
	}
}

// Every name is checked before anything is written.
func TestCopyChecksNamesFirst(t *testing.T) {
	to := newMem()
	if _, err := Copy(context.Background(), Map{"A_KEY": "a"}, to, []string{"A_KEY", "bad name"}); !errors.HasCode(err, ErrName) || to.sets != 0 {
		t.Fatalf("%v, %d writes", err, to.sets)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Copy(ctx, Map{"A_KEY": "a"}, newMem(), []string{"A_KEY"}); err == nil {
		t.Fatal("a cancelled copy went on")
	}
}

// An env file written for a container reads back the same, whatever the
// values; an existing file is never overwritten.
func TestWriteEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/keys.env"
	values := Map{"PEM": "-----BEGIN-----\nabc\r\n\t\"q\" \\ # not a comment\n-----END-----", "EMPTY": "", "PLAIN": "sk-1"}
	if err := WriteEnvFile(path, values); err != nil {
		t.Fatal(err)
	}
	res, err := EnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.(Map), values) {
		t.Fatalf("%q", res)
	}
	if err := WriteEnvFile(path, Map{"X": "y"}); err == nil {
		t.Fatal("an existing file was overwritten")
	}
	if err := WriteEnvFile(dir+"/bad.env", Map{"bad name": "v"}); err == nil {
		t.Fatal("a bad name was written")
	}
}

// A value no env line can hold is refused before anything is written,
// naming its key and not its value.
func TestWriteEnvFileRefusesWhatALineCannotHold(t *testing.T) {
	dir := t.TempDir()
	for name, value := range map[string]string{"NUL": "a\x00b", "BYTES": "\xff\xfe", "LONG": strings.Repeat("\\", maxValue/2)} {
		path := dir + "/" + name + ".env"
		err := WriteEnvFile(path, Map{name: value})
		if !errors.HasCode(err, ErrValue) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
		if _, statErr := os.Stat(path); statErr == nil {
			t.Errorf("%s: a file was left", name)
		}
	}
}
