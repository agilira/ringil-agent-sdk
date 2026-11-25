// dirs_test.go: the places of each system
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package dirs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agilira/go-errors"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// unixPaths skips a test of slash paths on Windows, where filepath
// writes them otherwise.
func unixPaths(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("slash paths: checked on Linux and macOS")
	}
}

func TestLinuxFollowsXDG(t *testing.T) {
	unixPaths(t)
	d := forSystem("linux", "app", "/home/u", envOf(nil))
	if d.Config != "/home/u/.config/app" || d.State != "/home/u/.local/state/app" || d.Data != "/home/u/.local/share/app" {
		t.Fatalf("%+v", d)
	}
	d = forSystem("linux", "app", "/home/u", envOf(map[string]string{"XDG_CONFIG_HOME": "/c", "XDG_STATE_HOME": "/s", "XDG_DATA_HOME": "/d"}))
	if d.Config != "/c/app" || d.State != "/s/app" || d.Data != "/d/app" {
		t.Fatalf("%+v", d)
	}
	// A relative XDG path is ignored, as the specification says.
	d = forSystem("linux", "app", "/home/u", envOf(map[string]string{"XDG_DATA_HOME": "rel/share"}))
	if d.Data != "/home/u/.local/share/app" {
		t.Fatalf("%+v", d)
	}
}

// One directory for all three variables: each place gets its own folder.
func TestSameXDGDirectoryStaysApart(t *testing.T) {
	unixPaths(t)
	d := forSystem("linux", "app", "/home/u", envOf(map[string]string{"XDG_CONFIG_HOME": "/x", "XDG_STATE_HOME": "/x", "XDG_DATA_HOME": "/x"}))
	if d.Config != "/x/app/config" || d.State != "/x/app/state" || d.Data != "/x/app/data" {
		t.Fatalf("%+v", d)
	}
	// Data inside the configuration's directory.
	d = forSystem("linux", "app", "/home/u", envOf(map[string]string{"XDG_DATA_HOME": "/home/u/.config/app/x"}))
	for _, own := range d.Own() {
		if Within(d.Data, own) {
			t.Fatalf("%+v", d)
		}
	}
}

func TestMac(t *testing.T) {
	unixPaths(t)
	d := forSystem("darwin", "app", "/Users/u", envOf(nil))
	if d.Config != "/Users/u/Library/Application Support/app/config" || d.Data != "/Users/u/Library/Application Support/app/data" {
		t.Fatalf("%+v", d)
	}
}

func TestWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows paths are absolute only there")
	}
	d := forSystem("windows", "app", `C:\Users\u`, envOf(map[string]string{"AppData": `C:\Users\u\AppData\Roaming`, "LocalAppData": `C:\Users\u\AppData\Local`}))
	if d.Config != `C:\Users\u\AppData\Roaming\app` || d.Data != `C:\Users\u\AppData\Local\app\data` {
		t.Fatalf("%+v", d)
	}
	// AppData and LocalAppData the same: the data stays out of the rest.
	d = forSystem("windows", "app", `C:\Users\u`, envOf(map[string]string{"AppData": `C:\h`, "LocalAppData": `C:\h`}))
	if d.Config != `C:\h\app\config` || d.State != `C:\h\app\state` || d.Data != `C:\h\app\data` {
		t.Fatalf("%+v", d)
	}
}

func TestPlacesAreApart(t *testing.T) {
	d, err := For("ringil")
	if err != nil {
		t.Fatal(err)
	}
	for _, own := range d.Own() {
		if Within(d.Inbox(), own) || Within(d.Data, own) {
			t.Fatalf("the data %s is inside %s", d.Data, own)
		}
	}
	if !Within(d.Inbox(), d.Data) {
		t.Fatal("the inbox is not in the data")
	}
}

func TestForRefusesBadNames(t *testing.T) {
	for _, name := range []string{"", "..", "../x", "a/b", `a\b`, ".hidden", "a b"} {
		if _, err := For(name); !errors.HasCode(err, ErrDirs) {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func TestWithin(t *testing.T) {
	cases := []struct {
		path, dir string
		want      bool
	}{
		{"/a/b", "/a", true}, {"/a", "/a", true}, {"/a/b/../c", "/a", true},
		{"/ab", "/a", false}, {"/a/../b", "/a", false}, {"/", "/a", false}, {"/a/..b", "/a", true},
	}
	for _, c := range cases {
		if got := Within(filepath.FromSlash(c.path), filepath.FromSlash(c.dir)); got != c.want {
			t.Errorf("Within(%q, %q) = %v", c.path, c.dir, got)
		}
	}
}

func TestMkdirAllIsPrivate(t *testing.T) {
	base := t.TempDir()
	d := Dirs{Config: filepath.Join(base, "c"), State: filepath.Join(base, "s"), Data: filepath.Join(base, "d")}
	if err := d.MkdirAll(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, p := range []string{d.Config, d.State, d.Data} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", p, st.Mode(), err)
		}
	}
}
