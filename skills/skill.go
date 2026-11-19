// skill.go: skills in the Agent Skills format
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package skills gives an agent procedures it loads when it needs them, in
// the Agent Skills format: a directory with a SKILL.md whose front matter
// holds a name and a description, followed by instructions, plus optional
// reference files and scripts. Skills published in that format install as
// they are.
//
// Disclosure is progressive. The system prompt lists only names and
// descriptions (Instructions); load_skill reads a skill's instructions;
// read_skill_file reads its reference files; run_skill_script runs its
// scripts in the sandbox.
//
// Installing a skill with scripts or files is the user's act, never the
// model's. The model may write a skill of instructions alone with
// create_skill (CreateTool), which the host asks the user about. Install
// and Create record a SHA-256 for every file, and every later read or
// run checks it, so a skill changed after its approval is refused.
package skills

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

// Error codes.
const (
	ErrInvalid  errors.ErrorCode = "SKILL_INVALID"
	ErrExists   errors.ErrorCode = "SKILL_EXISTS"
	ErrNotFound errors.ErrorCode = "SKILL_NOT_FOUND"
	ErrTampered errors.ErrorCode = "SKILL_TAMPERED"
	ErrIO       errors.ErrorCode = "SKILL_IO"
	ErrScript   errors.ErrorCode = "SKILL_SCRIPT"
)

// Info is what the model sees of a skill before loading it.
type Info struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// validName follows the Agent Skills rule: lowercase letters, digits and
// hyphens, 1-64 characters, no leading, trailing or double hyphen.
var nameRule = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func validName(name string) bool { return len(name) <= 64 && nameRule.MatchString(name) }

func (i Info) validate() error {
	switch {
	case !validName(i.Name):
		return errors.New(ErrInvalid, "skill names are 1-64 lowercase letters, digits and single hyphens")
	case strings.TrimSpace(i.Description) == "" || !utf8.ValidString(i.Description) || utf8.RuneCountInString(i.Description) > 1024:
		return errors.New(ErrInvalid, "a skill needs a description of 1-1024 characters")
	}
	return nil
}

// parse splits SKILL.md into its front matter fields and its body. Only
// the subset of YAML that skill files use is read: "key: value" lines,
// quoted values, and "|" or ">" block values; nested keys are skipped.
func parse(text string) (Info, string, error) {
	text = strings.TrimPrefix(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\ufeff")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Info{}, "", errors.New(ErrInvalid, "SKILL.md must start with a --- front matter block")
	}
	head, body, ok := cutFence(rest)
	if !ok {
		return Info{}, "", errors.New(ErrInvalid, "SKILL.md front matter is not closed with a --- line")
	}
	fields, dup := frontMatter(strings.Split(head, "\n"))
	if dup {
		return Info{}, "", errors.New(ErrInvalid, "SKILL.md front matter repeats a key")
	}
	info := Info{Name: fields["name"], Description: strings.TrimSpace(fields["description"])}
	return info, strings.TrimSpace(body), info.validate()
}

// cutFence splits at the first line that is exactly "---".
func cutFence(rest string) (string, string, bool) {
	if after, ok := strings.CutPrefix(rest, "---\n"); ok || rest == "---" {
		return "", after, true
	}
	for i := 0; ; {
		j := strings.Index(rest[i:], "\n---")
		if j < 0 {
			return "", "", false
		}
		end := i + j + 4
		if end == len(rest) || rest[end] == '\n' {
			return rest[:i+j], strings.TrimPrefix(rest[end:], "\n"), true
		}
		i = end
	}
}

func frontMatter(lines []string) (map[string]string, bool) {
	fields := map[string]string{}
	dup := false
	for i := 0; i < len(lines); i++ {
		if !topLevel(lines[i]) {
			continue // blank, nested or comment
		}
		key, value, ok := strings.Cut(lines[i], ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if style, block := blockStyle(value); block {
			var parts []string
			for i+1 < len(lines) && indented(lines[i+1]) {
				i++
				parts = append(parts, strings.TrimSpace(lines[i]))
			}
			value = joinBlock(parts, style == '>')
		}
		key = strings.TrimSpace(key)
		if _, seen := fields[key]; seen {
			dup = true
		}
		fields[key] = unquote(value)
	}
	return fields, dup
}

func topLevel(line string) bool { return line != "" && !indented(line) && line[0] != '#' }

func indented(line string) bool { return line == "" || line[0] == ' ' || line[0] == '\t' }

// blockStyle recognizes the YAML block scalars "|" and ">" (with "-").
func blockStyle(v string) (byte, bool) {
	switch v {
	case "|", "|-", ">", ">-":
		return v[0], true
	}
	return 0, false
}

func joinBlock(lines []string, folded bool) string {
	if folded {
		return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	}
	return strings.Join(lines, "\n")
}

// unquote reads YAML quoted scalars: 'it”s' and "a\"b\n".
func unquote(v string) string {
	switch {
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
		if s, err := strconv.Unquote(v); err == nil {
			return s
		}
		return v[1 : len(v)-1]
	}
	return v
}
