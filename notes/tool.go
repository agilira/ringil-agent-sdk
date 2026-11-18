// tool.go: the notes tool and its commands
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package notes

import (
	"context"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

type input struct {
	Command    string `json:"command" enum:"view,create,str_replace,insert,delete,rename" desc:"What to do"`
	Path       string `json:"path,omitempty" desc:"File or directory, such as /scratchpad/plan.md or /project"`
	FileText   string `json:"file_text,omitempty" desc:"create: the whole content"`
	OldStr     string `json:"old_str,omitempty" desc:"str_replace: text that appears exactly once"`
	NewStr     string `json:"new_str,omitempty" desc:"str_replace: its replacement"`
	InsertLine int    `json:"insert_line,omitempty" min:"0" desc:"insert: the line after which to insert; 0 is the top"`
	InsertText string `json:"insert_text,omitempty" desc:"insert: the text"`
	ViewStart  int    `json:"view_start,omitempty" min:"0" desc:"view: first line to show"`
	ViewEnd    int    `json:"view_end,omitempty" min:"0" desc:"view: last line to show"`
	NewPath    string `json:"new_path,omitempty" desc:"rename: the new path, in the same scope"`
}

// Tool returns the notes tool.
func (n *Notes) Tool() agent.Tool {
	desc := "Your notebooks of Markdown files: " + n.describeScopes() + ". " +
		"Keep working notes there: progress, findings, decisions, open questions, next steps; keep each scope's INDEX.md short and current, it is shown to you every turn. " +
		"Never store credentials. Commands: view (a directory lists its files, a file shows numbered lines), create, str_replace, insert, delete, rename."
	return agent.NewTool(DefaultName, desc, n.run)
}

func (n *Notes) run(ctx context.Context, in input) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s, rel, err := n.resolve(in.Path)
	if err != nil {
		return "", err
	}
	if in.Command == "view" {
		return view(s, rel, in)
	}
	if s.ReadOnly {
		return "", agent.ToolError(ErrDenied, "/"+s.Name+" is read-only for you.")
	}
	return change(ctx, s, rel, in, n)
}

var part = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}$`)

// resolve splits /<scope>/<rel> and checks every component: no hidden
// names, no traversal, nothing but plain names.
func (n *Notes) resolve(p string) (Scope, string, error) {
	clean := strings.Trim(p, "/")
	name, rel, _ := strings.Cut(clean, "/")
	s, ok := n.scopes[name]
	if !ok {
		return Scope{}, "", agent.ToolError(ErrInput, "Paths start with one of: "+n.describeScopes()+".")
	}
	if rel == "" {
		return s, ".", nil
	}
	for _, c := range strings.Split(rel, "/") {
		if !part.MatchString(c) {
			return Scope{}, "", agent.ToolError(ErrInput, "Use plain file names (letters, digits, _ - .), no hidden names or ..")
		}
	}
	return s, rel, nil
}

func view(s Scope, rel string, in input) (string, error) {
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return "", errors.Wrap(err, ErrIO, "cannot open the notes")
	}
	defer closeRoot(root)
	info, err := root.Lstat(rel)
	if err != nil {
		return "", agent.ToolError(ErrInput, "No such note: /"+s.Name+"/"+strings.TrimPrefix(rel, "."))
	}
	if info.IsDir() {
		return listing(root, s, rel)
	}
	text, err := readFile(s, rel)
	if err != nil {
		return "", err
	}
	return numbered(text, in.ViewStart, in.ViewEnd), nil
}

// listing shows the files of a directory, two levels deep, with sizes.
func listing(root *os.Root, s Scope, rel string) (string, error) {
	var lines []string
	walkErr := fs.WalkDir(root.FS(), rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == rel {
			return nil
		}
		if skip(rel, p, d) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if line := entryLine(s, p, d); line != "" {
			lines = append(lines, line)
		}
		return nil
	})
	if walkErr != nil {
		return "", errors.Wrap(walkErr, ErrIO, "cannot list the notes")
	}
	if len(lines) == 0 {
		return "/" + s.Name + " is empty.", nil
	}
	return strings.Join(lines, "\n"), nil
}

// skip leaves out hidden entries and anything deeper than two levels.
func skip(rel, p string, d fs.DirEntry) bool {
	return strings.HasPrefix(d.Name(), ".") || strings.Count(strings.TrimPrefix(p, rel+"/"), "/") > 1
}

func entryLine(s Scope, p string, d fs.DirEntry) string {
	if d.IsDir() {
		return "/" + s.Name + "/" + p + "/"
	}
	info, err := d.Info()
	if err != nil || !d.Type().IsRegular() {
		return ""
	}
	return "/" + s.Name + "/" + p + "  " + strconv.FormatInt(info.Size(), 10) + " bytes"
}

func numbered(text string, start, end int) string {
	lines := strings.Split(text, "\n")
	if start < 1 {
		start = 1
	}
	if end < 1 || end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	for i := start; i <= end; i++ {
		b.WriteString(strconv.Itoa(i) + "\t" + lines[i-1] + "\n")
	}
	if b.Len() == 0 {
		return "(no lines in that range)"
	}
	return b.String()
}

// change runs a writing command under the scope's lock, so that two
// agents sharing a scope never lose each other's edits.
func change(ctx context.Context, s Scope, rel string, in input, n *Notes) (string, error) {
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return "", errors.Wrap(err, ErrIO, "cannot open the notes")
	}
	defer closeRoot(root)
	lock, err := lockfile.Open(root, lockName)
	if err != nil {
		return "", errors.Wrap(err, ErrIO, "cannot lock the notes")
	}
	defer closeFile(lock)
	if err := lockfile.Lock(ctx, lock); err != nil {
		return "", err
	}
	switch in.Command {
	case "create":
		return create(root, s, rel, in.FileText)
	case "str_replace":
		return edit(root, s, rel, func(t string) (string, error) { return replaceOnce(t, in.OldStr, in.NewStr) })
	case "insert":
		return edit(root, s, rel, func(t string) (string, error) { return insertAt(t, in.InsertLine, in.InsertText) })
	case "delete":
		return remove(root, s, rel)
	case "rename":
		return rename(root, s, rel, in.NewPath, n)
	}
	return "", agent.ToolError(ErrInput, "Unknown command.")
}

func create(root *os.Root, s Scope, rel, text string) (string, error) {
	if rel == "." {
		return "", agent.ToolError(ErrInput, "Give a file path.")
	}
	if err := checkText(text); err != nil {
		return "", err
	}
	if _, err := root.Lstat(rel); os.IsNotExist(err) && countFiles(root) >= maxFiles {
		return "", agent.ToolError(ErrInput, "This scope has too many files; merge or delete some.")
	}
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return "", errors.Wrap(err, ErrIO, "cannot create the folder")
		}
	}
	if err := atomicfile.Write(root, rel, []byte(text)); err != nil {
		return "", errors.Wrap(err, ErrIO, "cannot write the note")
	}
	return "Saved /" + s.Name + "/" + rel + ".", nil
}

func edit(root *os.Root, s Scope, rel string, fn func(string) (string, error)) (string, error) {
	text, err := readFile(s, rel)
	if err != nil {
		return "", err
	}
	next, err := fn(text)
	if err != nil {
		return "", err
	}
	if err := checkText(next); err != nil {
		return "", err
	}
	if err := atomicfile.Write(root, rel, []byte(next)); err != nil {
		return "", errors.Wrap(err, ErrIO, "cannot write the note")
	}
	return "Updated /" + s.Name + "/" + rel + ".", nil
}

func replaceOnce(text, old, repl string) (string, error) {
	switch n := strings.Count(text, old); {
	case old == "":
		return "", agent.ToolError(ErrInput, "old_str is empty.")
	case n == 0:
		return "", agent.ToolError(ErrInput, "old_str does not appear in the note; view it and copy the text exactly.")
	case n > 1:
		return "", agent.ToolError(ErrInput, "old_str appears "+strconv.Itoa(n)+" times; include more context so it is unique.")
	}
	return strings.Replace(text, old, repl, 1), nil
}

func insertAt(text string, line int, add string) (string, error) {
	lines := strings.Split(text, "\n")
	if line < 0 || line > len(lines) {
		return "", agent.ToolError(ErrInput, "insert_line is past the end ("+strconv.Itoa(len(lines))+" lines).")
	}
	out := append(append(append([]string{}, lines[:line]...), strings.Split(add, "\n")...), lines[line:]...)
	return strings.Join(out, "\n"), nil
}

func remove(root *os.Root, s Scope, rel string) (string, error) {
	if rel == "." {
		return "", agent.ToolError(ErrInput, "Give a file or folder inside the scope.")
	}
	if _, err := root.Lstat(rel); err != nil {
		return "", agent.ToolError(ErrInput, "No such note.")
	}
	if err := root.RemoveAll(rel); err != nil {
		return "", errors.Wrap(err, ErrIO, "cannot delete the note")
	}
	return "Deleted /" + s.Name + "/" + rel + ".", nil
}

func rename(root *os.Root, s Scope, rel, newPath string, n *Notes) (string, error) {
	s2, rel2, err := n.resolve(newPath)
	if err != nil {
		return "", err
	}
	if s2.Name != s.Name || rel == "." || rel2 == "." {
		return "", agent.ToolError(ErrInput, "Rename within one scope, file to file.")
	}
	if _, err := root.Lstat(rel2); err == nil {
		return "", agent.ToolError(ErrInput, "The new path is taken.")
	}
	if dir := path.Dir(rel2); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return "", errors.Wrap(err, ErrIO, "cannot create the folder")
		}
	}
	if err := root.Rename(rel, rel2); err != nil {
		return "", agent.ToolError(ErrInput, "Cannot rename: no such note.")
	}
	return "Renamed to /" + s.Name + "/" + rel2 + ".", nil
}

func checkText(text string) error {
	if len(text) > MaxFile {
		return agent.ToolError(ErrInput, "A note is at most "+strconv.Itoa(MaxFile/1024)+" KiB; split it or summarize.")
	}
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return agent.ToolError(ErrInput, "Notes are text.")
	}
	return nil
}

// readFile reads a note that is a regular file of sane size.
func readFile(s Scope, rel string) (string, error) {
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return "", errors.Wrap(err, ErrIO, "cannot open the notes")
	}
	defer closeRoot(root)
	data, err := atomicfile.Read(root, rel, MaxFile)
	if err != nil {
		return "", agent.ToolError(ErrInput, "Cannot read /"+s.Name+"/"+rel+": no such note, or not a plain file.")
	}
	return string(data), nil
}

func countFiles(root *os.Root) int {
	n := 0
	walkErr := fs.WalkDir(root.FS(), ".", func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && !strings.HasPrefix(d.Name(), ".") {
			n++
		}
		return nil
	})
	if walkErr != nil {
		return maxFiles
	}
	return n
}

func closeRoot(r *os.Root) {
	if err := r.Close(); err != nil {
		return
	}
}

func closeFile(f *os.File) {
	if err := f.Close(); err != nil {
		return // the lock is released on close either way
	}
}
