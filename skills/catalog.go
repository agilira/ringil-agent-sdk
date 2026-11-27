// catalog.go: the installed skills and their verified files
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"bytes"
	"encoding/json"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

// Catalog is a directory of installed skills. It is safe for concurrent
// use: Reload swaps what it holds at once.
type Catalog struct {
	dir   string
	state atomic.Pointer[catalogState]
}

// catalogState is what one reading of the directory found.
type catalogState struct {
	skills map[string]manifest
	broken map[string]error
}

// Open reads the catalog in dir. A missing directory is an empty catalog;
// unfinished installations are ignored, and a damaged skill is left out
// and reported by Broken, so one bad skill does not stop the others. The
// directory must be the user's own and not writable by others: whoever
// writes it chooses what the agent is told.
func Open(dir string) (*Catalog, error) {
	c := &Catalog{dir: dir}
	if err := c.Reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// Reload reads the directory again, so a skill installed or removed since
// is listed, loaded and run from now on. On an error the catalog keeps
// what it held.
func (c *Catalog) Reload() error {
	st, err := readCatalog(c.dir)
	if err != nil {
		return err
	}
	c.state.Store(st)
	return nil
}

func readCatalog(dir string) (*catalogState, error) {
	st := &catalogState{skills: map[string]manifest{}, broken: map[string]error{}}
	if err := checkDir(dir); err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot read the skills directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot open the skills directory")
	}
	defer closeQuietly(root)
	for _, e := range entries {
		if !e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		m, err := readManifest(root, e.Name())
		if err != nil {
			st.broken[e.Name()] = err
			continue
		}
		st.skills[e.Name()] = m
	}
	return st, nil
}

func (c *Catalog) skills() map[string]manifest { return c.state.Load().skills }

// Broken returns the skills left out because they are damaged, with the
// reason; reinstalling fixes them.
func (c *Catalog) Broken() map[string]error { return c.state.Load().broken }

// checkDir refuses a catalog that is a symbolic link, someone else's, or
// writable by others.
func checkDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || lockfile.CheckPrivate(info) != nil {
		return errors.New(ErrIO, "the skills directory must be a real directory of yours, not writable by others: "+dir)
	}
	return nil
}

func readManifest(root *os.Root, name string) (manifest, error) {
	data, err := root.ReadFile(name + "/" + manifestName)
	if err != nil {
		return manifest{}, errors.Wrap(err, ErrTampered, "skill "+name+" has no manifest; reinstall it")
	}
	var m manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil || m.Version != 1 || m.Info.Name != name || m.Info.validate() != nil {
		return manifest{}, errors.New(ErrTampered, "skill "+name+" has a damaged manifest; reinstall it")
	}
	return m, nil
}

// List returns the installed skills by name.
func (c *Catalog) List() []Info {
	out := make([]Info, 0, len(c.skills()))
	for _, m := range c.skills() {
		out = append(out, m.Info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Source is where an installed skill came from (github.com/owner/repo/
// path@commit), empty for one installed from a local directory or
// written by the agent, or not installed.
func (c *Catalog) Source(name string) string { return c.skills()[name].Source }

// Instructions is the block to append to the system prompt: the skills'
// names and descriptions, one JSON object per line so that no description
// can break out of the block, and how to use them. Empty when no skill is
// installed.
func (c *Catalog) Instructions() string {
	if len(c.skills()) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n<skills>\nSkills are procedures installed by the user. When a task matches a skill's description, call load_skill with its name before acting, then follow its instructions; read_skill_file reads the files it mentions, and run_skill_script, when offered, runs its scripts.\n")
	for _, info := range c.List() {
		// The scripts are named here: small models otherwise guess a
		// script name instead of loading the skill first.
		line, err := json.Marshal(struct {
			Info
			Scripts []string `json:"scripts,omitempty"`
		}{info, c.scripts(info.Name)})
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteString("\n")
	}
	b.WriteString("</skills>")
	return b.String()
}

// scripts lists up to 10 files under scripts/ of a skill.
func (c *Catalog) scripts(name string) []string {
	files, _ := c.Files(name)
	var out []string
	for _, f := range files {
		if strings.HasPrefix(f, "scripts/") && len(out) < 10 {
			out = append(out, f)
		}
	}
	return out
}

// Load returns the instructions of a skill: the body of its SKILL.md.
func (c *Catalog) Load(name string) (string, error) {
	data, err := c.ReadFile(name, skillFile)
	if err != nil {
		return "", err
	}
	_, body, err := parse(string(data))
	return body, err
}

// Files lists a skill's files, SKILL.md included.
func (c *Catalog) Files(name string) ([]string, error) {
	m, ok := c.skills()[name]
	if !ok {
		return nil, notFound(name)
	}
	out := make([]string, 0, len(m.Files))
	for p := range m.Files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// resolve returns the path of file in skill name: the path itself, or the
// only file with that last component.
func (c *Catalog) resolve(name, file string) string {
	m, ok := c.skills()[name]
	if !ok {
		return file
	}
	if _, ok := m.Files[file]; ok {
		return file
	}
	found, _, _ := byBase(m.Files, file)
	return found
}

// byBase finds the only file whose last component is name.
func byBase(files map[string]string, name string) (string, string, bool) {
	if strings.Contains(name, "/") {
		return name, "", false
	}
	found, sum, n := name, "", 0
	for p, h := range files {
		if path.Base(p) == name {
			found, sum, n = p, h, n+1
		}
	}
	if n != 1 {
		return name, "", false
	}
	return found, sum, true
}

// listFiles names up to 20 files.
func listFiles(files []string) string {
	if len(files) > 20 {
		return strings.Join(files[:20], ", ") + " and " + strconv.Itoa(len(files)-20) + " more"
	}
	return strings.Join(files, ", ")
}

// ReadFile returns a file of a skill after checking it against the hash
// recorded at installation.
func (c *Catalog) ReadFile(name, file string) ([]byte, error) {
	m, ok := c.skills()[name]
	if !ok {
		return nil, notFound(name)
	}
	want, ok := m.Files[file]
	if !ok {
		// "stats.py" for scripts/stats.py: one file of that name is the
		// one meant.
		file, want, ok = byBase(m.Files, file)
	}
	if !ok {
		// Its files are named: a model that guessed a script name can
		// pick the real one instead of guessing again.
		files, _ := c.Files(name)
		return nil, errors.New(ErrNotFound, "skill "+name+" has no file "+file).WithUserMessage("Skill " + name + " has no file " + strconv.Quote(file) + "; its files: " + listFiles(files) + ".")
	}
	root, err := os.OpenRoot(c.dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot open the skills directory")
	}
	defer closeQuietly(root)
	data, err := readRegular(root, name+"/"+file)
	if err != nil {
		return nil, errors.Wrap(err, ErrTampered, "cannot read "+file+" of skill "+name)
	}
	if checksum(data) != want {
		return nil, errors.New(ErrTampered, "skill "+name+": "+file+" changed after installation; reinstall it").
			WithUserMessage("Skill " + name + " was changed after the user installed it and cannot be used.")
	}
	return data, nil
}

func notFound(name string) error {
	return errors.New(ErrNotFound, "no skill named "+name).WithUserMessage("No skill named " + name + " is installed.")
}
