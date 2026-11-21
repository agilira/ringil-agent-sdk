// repos.go: keeping git from running what a sandboxed command wrote
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import (
	"context"
	"os"
	"strings"
)

// gitGuarded are the entries of a .git directory through which git runs
// code or looks elsewhere for configuration: hooks, the configuration,
// info/, a redirected common directory, per-worktree configuration,
// submodules' and worktrees' own repositories. They stay read-only in the
// sandbox; one that did not exist must not appear.
var gitGuarded = []string{"hooks", "config", "info", "commondir", "config.worktree", "modules", "worktrees"}

// repo is what a .git held before a run.
type repo struct {
	dir bool
	has map[string]bool
}

// repoState maps each .git in the working directory (relative,
// slash-separated) to what it held.
type repoState map[string]repo

// scanRepos finds the repositories of dir with find (the working
// directory's own way of looking).
func scanRepos(ctx context.Context, dir string, find func(context.Context, []string) ([]string, error)) (repoState, error) {
	found, err := find(ctx, []string{".git"})
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }() // #nosec G104 -- read only
	state := repoState{}
	for _, rel := range found {
		info, err := root.Lstat(rel)
		if err != nil {
			continue
		}
		r := repo{dir: info.IsDir(), has: map[string]bool{}}
		for _, g := range gitGuarded {
			if _, err := root.Lstat(rel + "/" + g); err == nil {
				r.has[g] = true
			}
		}
		state[rel] = r
	}
	return state, nil
}

// guardRepos undoes, after a run, what would let git on the host run the
// command's code: guarded entries that appeared in a repository, a .git
// file pointing git elsewhere, and in a repository the command made, its
// hooks and any configuration beyond the plain settings git init and
// clone write. It returns what it undid.
func guardRepos(ctx context.Context, dir string, before repoState, find func(context.Context, []string) ([]string, error)) []string {
	after, err := scanRepos(ctx, dir, find)
	if err != nil {
		return []string{"the repositories could not be checked after the command: " + err.Error()}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return []string{"the repositories could not be checked after the command"}
	}
	defer func() { _ = root.Close() }() // #nosec G104 -- nothing buffered
	var notes []string
	for rel, now := range after {
		notes = append(notes, guardRepo(root, rel, now, before)...)
	}
	return notes
}

func guardRepo(root *os.Root, rel string, now repo, before repoState) []string {
	was, existed := before[rel]
	switch {
	case !now.dir && (!existed || was.dir):
		return []string{remove(root, rel, "a .git file sending git elsewhere")}
	case !now.dir:
		return nil
	case !existed:
		return newRepo(root, rel)
	}
	var notes []string
	for _, g := range gitGuarded {
		if now.has[g] && !was.has[g] {
			notes = append(notes, remove(root, rel+"/"+g, "made in the sandbox, where git would run or read it"))
		}
	}
	return notes
}

func remove(root *os.Root, rel, why string) string {
	if err := root.RemoveAll(rel); err != nil {
		return "could not remove " + rel + " (" + why + "): remove it before using git there"
	}
	return "removed " + rel + " (" + why + ")"
}

// newRepo neutralises a repository the command made: hooks other than
// git's samples, redirections, and configuration outside the plain set.
func newRepo(root *os.Root, rel string) []string {
	var notes []string
	if entries, err := readDir(root, rel+"/hooks"); err == nil {
		for _, e := range entries {
			if !strings.HasSuffix(e, ".sample") {
				notes = append(notes, remove(root, rel+"/hooks/"+e, "a hook made in the sandbox"))
			}
		}
	}
	for _, g := range []string{"commondir", "config.worktree", "modules", "worktrees"} {
		if _, err := root.Lstat(rel + "/" + g); err == nil {
			notes = append(notes, remove(root, rel+"/"+g, "made in the sandbox, where git would run or read it"))
		}
	}
	if dropped := sanitizeConfig(root, rel+"/config"); len(dropped) > 0 {
		notes = append(notes, "removed from "+rel+"/config the settings "+strings.Join(dropped, ", ")+" (only plain settings are kept in a repository made in the sandbox)")
	}
	return notes
}

func readDir(root *os.Root, rel string) ([]string, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // #nosec G104 -- read only
	return f.Readdirnames(-1)
}

// plainConfig are the settings kept in a repository the command made:
// what git init and clone write, none of which runs anything.
var plainConfig = map[string]bool{
	"core.repositoryformatversion": true, "core.filemode": true, "core.bare": true, "core.logallrefupdates": true,
	"core.ignorecase": true, "core.precomposeunicode": true, "core.symlinks": true, "core.autocrlf": true, "core.eol": true,
	"init.defaultbranch": true, "user.name": true, "user.email": true,
	"remote.*.url": true, "remote.*.fetch": true, "branch.*.remote": true, "branch.*.merge": true,
	"extensions.objectformat": true, "extensions.refstorage": true,
}

// sanitizeConfig rewrites a configuration keeping only plain settings,
// when it has others, and returns the names it dropped. Lines it cannot
// read as one setting (continuations) are dropped too.
func sanitizeConfig(root *os.Root, rel string) []string {
	data, err := root.ReadFile(rel)
	if err != nil {
		return nil
	}
	kept, dropped := filterConfig(string(data))
	if len(dropped) == 0 {
		return nil
	}
	if err := root.WriteFile(rel, []byte(kept), 0o600); err != nil {
		return append(dropped, "(the file could not be rewritten: remove "+rel+")")
	}
	return dropped
}

type configSection struct{ name, sub string }

func filterConfig(text string) (string, []string) {
	f := &configFilter{written: configSection{name: "\x00"}}
	for _, line := range strings.Split(text, "\n") {
		f.line(strings.TrimSpace(line))
	}
	return f.out.String(), f.dropped
}

// configFilter copies the plain settings of a configuration, line by line.
type configFilter struct {
	out          strings.Builder
	dropped      []string
	cur, written configSection
	continuation bool
}

func (f *configFilter) line(t string) {
	if f.continuation || strings.HasSuffix(t, "\\") {
		f.continuation = strings.HasSuffix(t, "\\")
		f.dropped = append(f.dropped, "(a continued line)")
		return
	}
	if strings.HasPrefix(t, "[") {
		end := strings.IndexByte(t, ']')
		if end < 0 {
			f.dropped = append(f.dropped, "(a malformed section)")
			f.cur = configSection{name: "\x00bad"}
			return
		}
		f.cur = parseSection(t[1:end])
		t = strings.TrimSpace(t[end+1:])
	}
	if t == "" || t[0] == '#' || t[0] == ';' {
		return
	}
	f.setting(t)
}

func (f *configFilter) setting(t string) {
	key, _, _ := strings.Cut(t, "=")
	key = strings.ToLower(strings.TrimSpace(key))
	name, pattern := f.cur.name+"."+key, f.cur.name+"."+key
	if f.cur.sub != "" {
		pattern = f.cur.name + ".*." + key
	}
	if !plainConfig[pattern] {
		f.dropped = append(f.dropped, name)
		return
	}
	if f.cur != f.written {
		f.out.WriteString(sectionHeader(f.cur))
		f.written = f.cur
	}
	f.out.WriteString("\t" + t + "\n")
}

// parseSection reads [name "sub"] and the old [name.sub].
func parseSection(h string) configSection {
	h = strings.TrimSpace(h)
	if name, sub, ok := strings.Cut(h, " "); ok {
		sub = strings.TrimSpace(sub)
		sub = strings.TrimSuffix(strings.TrimPrefix(sub, `"`), `"`)
		return configSection{name: strings.ToLower(name), sub: strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(sub)}
	}
	if name, sub, ok := strings.Cut(h, "."); ok {
		return configSection{name: strings.ToLower(name), sub: sub}
	}
	return configSection{name: strings.ToLower(h)}
}

func sectionHeader(s configSection) string {
	if s.sub == "" {
		return "[" + s.name + "]\n"
	}
	return "[" + s.name + " \"" + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s.sub) + "\"]\n"
}
