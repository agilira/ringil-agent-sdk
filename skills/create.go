// create.go: skills the agent writes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// MaxInstructions bounds a skill's instructions written with Create:
// within what a chat shows of an approval in full.
const MaxInstructions = 10 << 10

const replacedPrefix = ".replace-"

// Create writes a skill made only of instructions (a SKILL.md, no
// scripts, no other files) into the catalog directory dir, recorded like
// an installed one and marked as the agent's. With replace, it takes the
// place of a skill by that name, but only of one the agent wrote, still
// instructions alone: the skills the user installed are the user's to
// change. What does not print (controls, zero-width and bidi characters)
// is refused: what the user approves is what is written.
func Create(ctx context.Context, dir string, info Info, instructions string, replace bool) error {
	if err := checkCreate(info, instructions); err != nil {
		return err
	}
	src, err := os.MkdirTemp("", "ringil-skill-")
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot stage the skill")
	}
	defer func() { _ = os.RemoveAll(src) }()
	if err := os.WriteFile(src+string(os.PathSeparator)+skillFile, []byte(skillText(info, instructions)), 0o600); err != nil {
		return errors.Wrap(err, ErrIO, "cannot stage the skill")
	}
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot stage the skill")
	}
	defer closeQuietly(srcRoot)
	root, err := openCatalog(dir)
	if err != nil {
		return err
	}
	defer closeQuietly(root)
	if !replace {
		return publish(ctx, srcRoot, root, info, origin{agent: true})
	}
	return republish(ctx, srcRoot, root, info)
}

func checkCreate(info Info, instructions string) error {
	switch {
	case strings.ContainsAny(info.Description, "\r\n"):
		return errors.New(ErrInvalid, "a skill's description is one line")
	case strings.TrimSpace(instructions) == "" || !utf8.ValidString(instructions):
		return errors.New(ErrInvalid, "a skill needs instructions")
	case len(instructions) > MaxInstructions:
		return errors.New(ErrInvalid, "a skill's instructions are at most 10 KiB")
	case hidden(info.Description) || hidden(instructions):
		return errors.New(ErrInvalid, "a skill holds characters that do not show (controls, zero-width or bidi marks)")
	}
	return info.validate()
}

// hidden: s holds a character that does not print, other than line
// breaks and tabs.
func hidden(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029)
	})
}

// skillText is the SKILL.md of a skill: its front matter (the
// description quoted, so that nothing in it reads as another field) and
// its instructions. The name is quoted too: "true" or "123" are names
// here, not YAML's booleans or numbers.
func skillText(info Info, instructions string) string {
	return "---\nname: " + strconv.Quote(info.Name) + "\ndescription: " + strconv.Quote(info.Description) + "\n---\n\n" + strings.TrimSpace(instructions) + "\n"
}

// republish replaces a skill the agent wrote, or publishes a new one:
// the new one is staged, the old one moved aside, the new one renamed in.
func republish(ctx context.Context, src, root *os.Root, info Info) error {
	if _, err := root.Lstat(info.Name); err != nil {
		return publish(ctx, src, root, info, origin{agent: true})
	}
	if err := agentsOwn(root, info.Name); err != nil {
		return err
	}
	old := replacedPrefix + strings.ToLower(rand.Text()[:12])
	if err := root.Rename(info.Name, old); err != nil {
		return errors.Wrap(err, ErrIO, "cannot replace the skill")
	}
	if err := publish(ctx, src, root, info, origin{agent: true}); err != nil {
		if rerr := root.Rename(old, info.Name); rerr != nil {
			return installError(err).WithContext("restore_error", rerr.Error())
		}
		return err
	}
	_ = root.RemoveAll(old) // a leftover is hidden; a later install or create clears it
	return nil
}

// agentsOwn checks that a skill is one the agent wrote and still only
// its SKILL.md, in its manifest and on disk.
func agentsOwn(root *os.Root, name string) error {
	data, err := root.ReadFile(name + "/" + manifestName)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot read skill "+name)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return errors.Wrap(err, ErrIO, "cannot read skill "+name)
	}
	if !m.Agent {
		return errors.New(ErrExists, "skill "+name+" was installed by the user: only the user can change it")
	}
	entries, err := fs.ReadDir(root.FS(), name)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot read skill "+name)
	}
	for _, e := range entries {
		if e.Name() != skillFile && e.Name() != manifestName {
			return errors.New(ErrExists, "skill "+name+" holds other files ("+e.Name()+"): only the user can change it")
		}
	}
	return nil
}

type createIn struct {
	Name         string `json:"name" desc:"The skill's name: lowercase letters, digits and single hyphens, e.g. triage-mail"`
	Description  string `json:"description" desc:"One line: what the skill does and when to use it; it is all that is seen of it before loading"`
	Instructions string `json:"instructions" desc:"The steps to follow, in Markdown, as for a capable colleague"`
	Replace      bool   `json:"replace,omitempty" desc:"Replace the skill by this name (only one made of instructions alone)"`
}

// CreateTool is create_skill: the agent writes a skill of instructions
// into the catalog directory dir, for later runs (and for triggers) to
// follow. It is marked destructive: the host asks the user, showing what
// will be written, as it is a lasting change to what the agent does.
// created, when not nil, hears of each skill written.
func CreateTool(dir string, created func(Info)) agent.Tool {
	return agent.NewTool("create_skill",
		"Write a skill: instructions for a task, kept for later runs and triggers, loaded with load_skill when the task comes. "+
			"Use it when the user asks for a lasting procedure. Instructions only: no scripts.",
		func(ctx context.Context, in createIn) (string, error) {
			info := Info{Name: in.Name, Description: strings.TrimSpace(in.Description)}
			if err := Create(ctx, dir, info, in.Instructions, in.Replace); err != nil {
				return "", err
			}
			if created != nil {
				created(info)
			}
			return "Skill " + info.Name + " written: later runs list it and load it with load_skill.", nil
		}, agent.Destructive())
}
