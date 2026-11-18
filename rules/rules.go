// rules.go: the instructions people wrote for agents in their projects
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package rules reads the instructions people keep for coding agents in
// their projects, so an agent built on this SDK follows the same rules as
// the ones they already use: AGENTS.md, the open standard, and the files
// of other agents (CLAUDE.md, GEMINI.md, Copilot's, Cursor's, Windsurf's).
//
// Files are read from the repository root (the nearest directory with a
// .git) down to the working directory, so that a closer file refines a
// broader one, plus the user's global file. Links are followed, as
// people often link AGENTS.md to CLAUDE.md, but never out of the
// repository or the global directory and never to secret files; the same
// text is not repeated, and the total is bounded.
package rules

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agilira/ringil-agent-sdk/tools/files"
)

// DefaultFiles are read in each directory, in this order: AGENTS.md
// first, then other agents' files.
var DefaultFiles = []string{
	"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md", "GEMINI.md", "HERMES.md",
	".github/copilot-instructions.md", ".cursorrules", ".windsurfrules", ".clinerules",
	".cursor/rules", // a directory: its .md and .mdc files
}

// Bounds: rules are read into every turn.
const (
	maxFile  = 32 << 10
	maxTotal = 96 << 10
)

// Config says where to look.
type Config struct {
	// Dir is the working directory; required.
	Dir string
	// Global is the user's own rules file for every project, if any.
	Global string
	// Files overrides DefaultFiles.
	Files []string
	// Stop is where the walk up from Dir ends; default the nearest
	// directory holding .git, or Dir alone when there is none.
	Stop string
}

// File is one rules file that was read.
type File struct {
	Path string
	Text string
}

// Load reads the rules, broadest first.
func Load(cfg Config) []File {
	l := &loader{cfg: cfg, seen: map[string]bool{}}
	if cfg.Global != "" {
		l.read(cfg.Global, filepath.Dir(cfg.Global))
	}
	stop := cfg.Stop
	if stop == "" {
		stop = repoRoot(cfg.Dir)
	}
	for _, dir := range chain(stop, cfg.Dir) {
		l.dir(dir, stop)
	}
	return l.out
}

type loader struct {
	cfg   Config
	seen  map[string]bool // resolved paths and texts already read
	out   []File
	total int
}

func (l *loader) names() []string {
	if l.cfg.Files != nil {
		return l.cfg.Files
	}
	return DefaultFiles
}

func (l *loader) dir(dir, stop string) {
	for _, name := range l.names() {
		p := filepath.Join(dir, filepath.FromSlash(name))
		info, err := os.Stat(p)
		switch {
		case err != nil:
		case info.IsDir():
			l.ruleDir(p, stop)
		default:
			l.read(p, stop)
		}
	}
}

// ruleDir reads the Markdown rules of a directory such as .cursor/rules.
func (l *loader) ruleDir(dir, stop string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); ext == ".md" || ext == ".mdc" {
			l.read(filepath.Join(dir, e.Name()), stop)
		}
	}
}

// read adds one file if it resolves inside within, is a regular file and
// not a secret, and says something new.
func (l *loader) read(p, within string) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil || l.seen[real] || !inside(real, within) || files.Excluded(real, nil) {
		return
	}
	l.seen[real] = true
	text, ok := readRegular(real)
	if text = strings.TrimSpace(text); ok && text != "" {
		l.add(p, text)
	}
}

// add keeps a new text within the total bound.
func (l *loader) add(p, text string) {
	if l.seen["text:"+text] || l.total >= maxTotal {
		return
	}
	l.seen["text:"+text] = true
	if room := maxTotal - l.total; len(text) > room {
		text = strings.ToValidUTF8(text[:room], "") + "\n[...]"
	}
	l.total += len(text)
	l.out = append(l.out, File{Path: p, Text: text})
}

func readRegular(p string) (string, bool) {
	f, err := os.OpenFile(p, os.O_RDONLY|nonBlock, 0) // #nosec G304 -- a rules file of the user's project, checked by the caller
	if err != nil {
		return "", false
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			return
		}
	}()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFile))
	if err != nil {
		return "", false
	}
	text := strings.ToValidUTF8(string(data), "")
	if info.Size() > maxFile {
		text += "\n[...]"
	}
	return text, true
}

// repoRoot is the nearest directory from dir upwards holding .git, or dir.
func repoRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// chain lists the directories from top down to dir.
func chain(top, dir string) []string {
	var out []string
	for d := dir; ; d = filepath.Dir(d) {
		out = append([]string{d}, out...)
		if d == top || filepath.Dir(d) == d {
			break
		}
	}
	return out
}

func inside(p, dir string) bool {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(realDir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Context returns a Config.Context hook that reads the rules at the start
// of every run, so edits apply at once.
func Context(cfg Config) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return Render(Load(cfg)), nil }
}

// Render presents the rules to the model: the user's instructions, to
// follow, with where each came from.
func Render(fs []File) string {
	if len(fs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<project-rules>\nThe user's rules for this project, from the files below (broadest first; a later, closer file refines an earlier one). Follow them. What the user asks in this conversation comes first. Files written for other agents may name tools or commands you do not have: follow their intent with your own tools.\n")
	for i, f := range fs {
		b.WriteString("\n## " + strconv.Itoa(i+1) + ". " + filepath.ToSlash(f.Path) + "\n\n")
		b.WriteString(strings.ReplaceAll(f.Text, "</project-rules>", "</project_rules>"))
		b.WriteString("\n")
	}
	b.WriteString("</project-rules>")
	return b.String()
}
