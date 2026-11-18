// envfile.go: private .env files
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

// Bounds on an env file; far above any real one.
const (
	maxEnvFile = 1 << 20
	maxEnvLine = 64 << 10
)

// ParseEnv reads KEY=VALUE lines in the common .env syntax:
//
//	# a comment
//	KEY=value            # trailing comment, not part of the value
//	export KEY=value
//	KEY="double quoted, with \n \t \" \\ escapes"   # comment
//	KEY='single quoted, literal # not a comment'
//	KEY=                 # empty value
//
// Unquoted values end at the first " #" and are trimmed. There is no
// variable expansion and no command substitution. The last assignment of a
// name wins. Errors name the line, never its content.
func ParseEnv(r io.Reader) (map[string]string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxEnvFile+1))
	if err != nil || len(data) > maxEnvFile || !utf8.Valid(data) {
		return nil, errors.New(ErrSource, "env file is unreadable, too large or not UTF-8")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 4096), maxEnvLine)
	values := map[string]string{}
	for line := 1; scanner.Scan(); line++ {
		name, value, ok, err := parseLine(scanner.Text())
		if err != nil {
			return nil, errors.Wrap(err, ErrSource, "env file line "+strconv.Itoa(line))
		}
		if ok {
			values[name] = value
		}
	}
	if scanner.Err() != nil {
		return nil, errors.New(ErrSource, "env file has a line longer than 64 KiB")
	}
	return values, nil
}

func syntaxError(msg string) error { return errors.New(ErrSource, msg) }

func parseLine(raw string) (string, string, bool, error) {
	line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
	if strings.ContainsRune(line, 0) {
		return "", "", false, syntaxError("NUL byte")
	}
	if line == "" || line[0] == '#' {
		return "", "", false, nil
	}
	name, value, found := strings.Cut(stripExport(line), "=")
	name = strings.TrimSpace(name)
	if !found || ValidateName(name) != nil {
		return "", "", false, syntaxError("expected NAME=value")
	}
	parsed, err := parseValue(strings.TrimLeft(value, " \t"))
	return name, parsed, err == nil, err
}

func stripExport(line string) string {
	rest, ok := strings.CutPrefix(line, "export")
	if ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
		return strings.TrimSpace(rest)
	}
	return line
}

func parseValue(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	switch v[0] {
	case '"':
		return parseDouble(v[1:])
	case '\'':
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 {
			return "", syntaxError("unterminated single quote")
		}
		return v[1 : end+1], onlyComment(v[end+2:])
	}
	// An unquoted value ends where a comment starts: "#" at the start or
	// after whitespace. A "#" inside a word (a URL fragment, a password)
	// stays part of the value.
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	} else if i := strings.Index(v, "\t#"); i >= 0 {
		v = v[:i]
	} else if v[0] == '#' {
		v = ""
	}
	return strings.TrimRight(v, " \t"), nil
}

func parseDouble(v string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '"':
			return b.String(), onlyComment(v[i+1:])
		case c == '\\' && i+1 < len(v):
			i++
			b.WriteString(unescape(v[i]))
		default:
			b.WriteByte(c)
		}
	}
	return "", syntaxError("unterminated double quote")
}

func unescape(c byte) string {
	switch c {
	case 'n':
		return "\n"
	case 't':
		return "\t"
	case 'r':
		return "\r"
	}
	return string(c)
}

// onlyComment accepts what follows a closing quote: nothing, or a comment.
func onlyComment(rest string) error {
	rest = strings.TrimSpace(rest)
	if rest == "" || rest[0] == '#' {
		return nil
	}
	return syntaxError("unexpected text after a quoted value")
}

// EnvFile loads a private env file once and resolves names from it. The
// file must be a regular file of the current user, not a symlink, and not
// readable by group or others: a world-readable credential file is refused,
// not silently used.
func EnvFile(path string) (Resolver, error) {
	data, err := readPrivate(path)
	if err != nil {
		return nil, err
	}
	values, err := ParseEnv(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return Map(values), nil
}

func readPrivate(path string) ([]byte, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New(ErrSource, "the env file must not be a symlink")
	}
	// The path is chosen by the operator; opening it through a root on its
	// directory still keeps a link in its last component from leading out.
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, errors.New(ErrSource, "cannot open the env file's directory")
	}
	data, rerr := readIn(root, filepath.Base(path))
	if cerr := root.Close(); rerr == nil && cerr != nil {
		rerr = errors.New(ErrSource, "cannot close the env file's directory")
	}
	return data, rerr
}

func readIn(root *os.Root, name string) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return nil, errors.New(ErrSource, "cannot open the env file (it must exist and not be a symlink)")
	}
	data, rerr := readChecked(f)
	if cerr := f.Close(); rerr == nil && cerr != nil {
		rerr = errors.New(ErrSource, "cannot close the env file")
	}
	return data, rerr
}

func readChecked(f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New(ErrSource, "the env file must be a regular file")
	}
	if err := checkOwner(info); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxEnvFile+1))
	if err != nil {
		return nil, errors.New(ErrSource, "cannot read the env file")
	}
	return data, nil
}
