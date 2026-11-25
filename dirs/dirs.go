// dirs.go: where an agent's files go
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package dirs gives an agent the standard places for its files, on each
// system, so that the agent's own files are kept from its tools and what
// it works on is within their reach:
//
//   - Config: the settings, the system prompt, the skills, the MCP
//     servers. The agent's own: never granted to its tools.
//   - State: sessions, memory, audit, tokens. The agent's own too.
//   - Data: what the agent works on: the documents sent in the chats
//     (Inbox), the files it makes. Its tools may read here.
//
// Pass Own to the sandbox and the file tools as the paths they never
// grant (bash's Never, the files' Protected), and Inbox to the Bridge.
// Updating the program replaces only the binary: none of these is next
// to it.
//
//	Linux    $XDG_CONFIG_HOME/app   $XDG_STATE_HOME/app            $XDG_DATA_HOME/app
//	         (~/.config/app)        (~/.local/state/app)           (~/.local/share/app)
//	macOS    ~/Library/Application Support/app/{config,state,data}
//	Windows  %AppData%\app          %LocalAppData%\app\state       %LocalAppData%\app\data
package dirs

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"

	"github.com/agilira/go-errors"
)

// ErrDirs: the places cannot be found, or a name is not usable.
const ErrDirs errors.ErrorCode = "DIRS"

// Dirs are an agent's places.
type Dirs struct {
	Config, State, Data string
}

var appName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// For returns the places of the app named app (letters, digits, . _ -)
// for the current user. They are not created: MkdirAll does it.
func For(app string) (Dirs, error) {
	if !appName.MatchString(app) || strings.Contains(app, "..") {
		return Dirs{}, errors.New(ErrDirs, "an app name is 1-64 letters, digits, . _ or -")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return Dirs{}, errors.New(ErrDirs, "the user has no home directory")
	}
	return forSystem(runtime.GOOS, app, home, os.Getenv), nil
}

func forSystem(goos, app, home string, env func(string) string) Dirs {
	switch goos {
	case "darwin", "ios":
		base := filepath.Join(home, "Library", "Application Support", app)
		return Dirs{Config: filepath.Join(base, "config"), State: filepath.Join(base, "state"), Data: filepath.Join(base, "data")}
	case "windows":
		roaming := absOr(env("AppData"), filepath.Join(home, "AppData", "Roaming"))
		local := absOr(env("LocalAppData"), filepath.Join(home, "AppData", "Local"))
		return apart(Dirs{Config: filepath.Join(roaming, app), State: filepath.Join(local, app, "state"), Data: filepath.Join(local, app, "data")})
	}
	return apart(Dirs{
		Config: filepath.Join(absOr(env("XDG_CONFIG_HOME"), filepath.Join(home, ".config")), app),
		State:  filepath.Join(absOr(env("XDG_STATE_HOME"), filepath.Join(home, ".local", "state")), app),
		Data:   filepath.Join(absOr(env("XDG_DATA_HOME"), filepath.Join(home, ".local", "share")), app),
	})
}

// apart keeps the three places from nesting when the variables name the
// same directory (XDG ones on Linux, AppData and LocalAppData on
// Windows): the configuration then gets a folder of its own, and so do
// the state and the data if they still meet, so the data never falls
// inside the agent's own places.
func apart(d Dirs) Dirs {
	if !nested(d.Data, d.Config) && !nested(d.Data, d.State) && !nested(d.State, d.Config) {
		return d
	}
	d.Config = filepath.Join(d.Config, "config")
	if nested(d.State, d.Data) {
		d.State, d.Data = filepath.Join(d.State, "state"), filepath.Join(d.Data, "data")
	}
	return d
}

// nested: one of a and b is the other or inside it.
func nested(a, b string) bool { return Within(a, b) || Within(b, a) }

// absOr is v when it is an absolute path, as the XDG rules want; else def.
func absOr(v, def string) string {
	if v != "" && filepath.IsAbs(v) {
		return filepath.Clean(v)
	}
	return def
}

// Program are the places of the running program, named after its
// executable (letters, digits, . _ -; "agent" when it has none usable):
// what every package of the SDK uses when the host sets no place.
func Program() (Dirs, error) { return For(ProgramName()) }

// ProgramName is the executable's name as For takes it.
func ProgramName() string {
	exe, err := os.Executable()
	if err != nil {
		return "agent"
	}
	name := strings.Trim(strings.Map(nameRune, strings.TrimSuffix(filepath.Base(exe), filepath.Ext(exe))), ".-_")
	if name == "" || !appName.MatchString(name) {
		return "agent"
	}
	return name
}

// nameRune keeps the ASCII letters, digits and . _ - of a name.
func nameRune(r rune) rune {
	if r < 128 && (r == '.' || r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
		return r
	}
	return '-'
}

// Inbox is where the Bridge keeps the documents sent in the chats.
func (d Dirs) Inbox() string { return filepath.Join(d.Data, "inbox") }

// Own are the agent's own places, Config and State: the paths its tools
// are never granted.
func (d Dirs) Own() []string { return []string{d.Config, d.State} }

// MkdirAll creates the three places, private to the user.
func (d Dirs) MkdirAll() error {
	for _, p := range []string{d.Config, d.State, d.Data} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return errors.Wrap(err, ErrDirs, "cannot create "+p)
		}
	}
	return nil
}

// Within reports whether path is dir or inside it, by their cleaned
// absolute forms (case-insensitively on Windows and macOS).
func Within(path, dir string) bool {
	p, err1 := filepath.Abs(path)
	d, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(fold(d), fold(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func fold(p string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(p)
	}
	return p
}
