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
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/agilira/ringil-agent-sdk/tools/files"
)

// DefaultFiles are read in each directory, in this order: AGENTS.md
// first, then other agents' files.
// A name ending in "/" is a folder whose .md and .mdc files are read.
var DefaultFiles = []string{
	"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md", "GEMINI.md",
	".github/copilot-instructions.md", ".cursorrules", ".windsurfrules", ".clinerules",
	".cursor/rules/",
}

// rootOnly are read only at the repository root, as their agents do.
var rootOnly = map[string]bool{".github/copilot-instructions.md": true}

// Default bounds: rules are read into every turn.
const (
	DefaultMaxFile  = 32 << 10
	DefaultMaxTotal = 96 << 10
)

// Config says where to look.
type Config struct {
	// Dir is the working directory; required.
	Dir string
	// Global is the user's own rules file for every project, if any.
	Global string
	// Files overrides DefaultFiles.
	Files []string
	// MaxFile and MaxTotal bound what is read of one file and of them all
	// (DefaultMaxFile, DefaultMaxTotal): rules go into every turn.
	MaxFile, MaxTotal int
	// Stop is where the walk up from Dir ends; default the nearest
	// directory holding .git, or Dir alone when there is none.
	Stop string
}

// File is one rules file that was read.
type File struct {
	Path string
	Text string
}

// Load reads the rules, broadest first. When they exceed the total
// bound, the closest files are kept and the broadest cut: a folder's own
// rules are the most specific.
func Load(cfg Config) []File {
	if cfg.MaxFile <= 0 {
		cfg.MaxFile = DefaultMaxFile
	}
	if cfg.MaxTotal <= 0 {
		cfg.MaxTotal = DefaultMaxTotal
	}
	l := &loader{cfg: cfg, seen: map[string]bool{}}
	if cfg.Global != "" {
		l.read(cfg.Global, "")
	}
	stop := cfg.Stop
	if stop == "" {
		stop = trustedRoot(cfg.Dir)
	}
	for _, dir := range chain(stop, cfg.Dir) {
		l.dir(dir, stop)
	}
	return budget(l.out, cfg.MaxTotal)
}

// trustedRoot is the repository root, unless that root is the home or
// above it (a home kept in git): then only the working directory counts,
// so a project's links cannot reach the user's own files.
func trustedRoot(dir string) string {
	root := repoRoot(dir)
	if home, err := os.UserHomeDir(); err == nil && home != "" && inside(resolve(home), root) {
		return dir
	}
	return root
}

// resolve is p with links resolved when it exists: /var is /private/var
// on macOS, and short names differ on Windows.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

type loader struct {
	cfg  Config
	seen map[string]bool // resolved paths and texts already read
	out  []File
}

func (l *loader) names() []string {
	if l.cfg.Files != nil {
		return l.cfg.Files
	}
	return DefaultFiles
}

func (l *loader) dir(dir, stop string) {
	override := exists(filepath.Join(dir, "AGENTS.override.md"))
	for _, name := range l.names() {
		if (rootOnly[name] && dir != stop) || (override && name == "AGENTS.md") {
			continue // Codex reads the override instead of AGENTS.md
		}
		p := filepath.Join(dir, filepath.FromSlash(strings.TrimSuffix(name, "/")))
		if strings.HasSuffix(name, "/") {
			l.ruleDir(p, stop)
		} else {
			l.read(p, stop)
		}
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ruleDir reads the Markdown rules of a folder such as .cursor/rules.
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

// read adds one file if it is a regular file with a single name, not a
// secret, and says something new. A link must resolve to Markdown, and,
// for project files, inside within; the global file is the user's own and
// may link anywhere.
func (l *loader) read(p, within string) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil || l.seen[real] || !l.allowed(p, real, within) {
		return
	}
	l.seen[real] = true
	text, ok := readRegular(real, l.cfg.MaxFile)
	if text = strings.TrimSpace(text); ok && text != "" && !l.seen["text:"+text] {
		l.seen["text:"+text] = true
		l.out = append(l.out, File{Path: p, Text: text})
	}
}

func (l *loader) allowed(p, real, within string) bool {
	if files.Excluded(real, nil) {
		return false
	}
	if real == p || real == mustAbs(p) {
		return true // not a link
	}
	ext := filepath.Ext(real)
	return (ext == ".md" || ext == ".mdc") && (within == "" || inside(real, within))
}

func mustAbs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		if r, err := filepath.EvalSymlinks(filepath.Dir(a)); err == nil {
			return filepath.Join(r, filepath.Base(a))
		}
	}
	return p
}

// budget keeps the total within total, filling from the closest file back
// to the broadest; a file that does not fit is cut or left out with a
// mark, so the model knows something is missing.
func budget(fs []File, total int) []File {
	left := total
	for i := len(fs) - 1; i >= 0; i-- {
		switch {
		case len(fs[i].Text) <= left:
			left -= len(fs[i].Text)
		case left > 1024:
			fs[i].Text = strings.ToValidUTF8(fs[i].Text[:left], "") + "\n[... cut: the rules are too long]"
			left = 0
		default:
			fs[i].Text = "[left out: the rules are too long]"
		}
	}
	return fs
}

func readRegular(p string, max int) (string, bool) {
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
	data, err := io.ReadAll(io.LimitReader(f, int64(max)))
	if err != nil {
		return "", false
	}
	if !singleName(info) {
		return "", false // a hard link could bring any file in under a rules name
	}
	text := strings.Map(dropNUL, strings.ToValidUTF8(string(data), ""))
	if info.Size() > int64(max) {
		text += "\n[...]"
	}
	return text, true
}

func dropNUL(r rune) rune {
	if r == 0 {
		return -1
	}
	return r
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
	rel, err := filepath.Rel(realDir, resolve(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

var closeTag = regexp.MustCompile(`(?i)<\s*/\s*project-rules\s*>`)

// SafePath shows a path on one line: control characters, which could
// forge headers or terminal lines, are dropped.
func SafePath(p string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == 0x2028 || r == 0x2029 {
			return -1
		}
		return r
	}, filepath.ToSlash(p))
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
	b.WriteString("<project-rules>\nThe user's rules for this project, from the files below, broadest first: a file in a deeper folder refines those above it; files in the same folder are written for different agents and apply together. Follow them. What the user asks in this conversation comes first. Files written for other agents may name tools or commands you do not have: follow their intent with your own tools.\n")
	for i, f := range fs {
		b.WriteString("\n## " + strconv.Itoa(i+1) + ". " + SafePath(f.Path) + "\n\n")
		b.WriteString(closeTag.ReplaceAllString(f.Text, "</project_rules>"))
		b.WriteString("\n")
	}
	b.WriteString("</project-rules>")
	return b.String()
}
