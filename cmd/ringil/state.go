// state.go: artifacts, and backups of the agent's state, in the terminal
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/artifact"
	"github.com/agilira/ringil-agent-sdk/backup"
)

func (c *cli) artifacts() (*artifact.Local, error) {
	base, err := c.configDir()
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "no user config directory")
	}
	return artifact.NewLocal(filepath.Join(base, "ringil", "artifacts"))
}

// artifactScope is the project of the start directory: the agent, its
// workers and its workflows publish and read there.
func (c *cli) artifactScope() (string, error) {
	cwd, err := c.cwd()
	if err != nil {
		return "", err
	}
	return projectKey(cwd), nil
}

// artifactTools: the agent publishes in the project's scope and reads
// what its workers published; a worker publishes in the workers' scope
// and reads the project's, so a worker can never replace what the agent
// reads as the project's latest.
func (c *cli) artifactTools(worker bool) []agent.Tool {
	store, err := c.artifacts()
	if err != nil {
		return nil
	}
	project, err := c.artifactScope()
	if err != nil {
		return nil
	}
	work := project + "-work"
	if worker {
		return artifact.Tools(store, artifact.Access{Scope: work, By: "worker", Publish: true, Read: []string{project}})
	}
	return artifact.Tools(store, artifact.Access{Scope: project, By: "agent", Publish: true, Read: []string{work}})
}

func (c *cli) cmdArtifacts(ctx *orpheus.Context) error {
	store, err := c.artifacts()
	if err != nil {
		return err
	}
	scope, err := c.artifactScope()
	if err != nil {
		return err
	}
	total := 0
	for _, sc := range []string{scope, scope + "-work"} {
		list, err := store.List(ctx.Context(), sc)
		if err != nil {
			return err
		}
		total += len(list)
		for _, a := range list {
			where := ""
			if sc != scope {
				where = "  (workers)"
			}
			if err := c.line(clean(a.Name) + " v" + strconv.Itoa(a.Version) + "  " + clean(a.MediaType) + "  " + strconv.FormatInt(a.Size, 10) + " bytes  " + a.Created.Format("2006-01-02 15:04") + "  " + short(a.By) + where); err != nil {
				return err
			}
		}
	}
	if total == 0 {
		return c.line("no artifacts for this project")
	}
	return nil
}

// cmdArtifactShow writes an artifact's content to standard output.
func (c *cli) cmdArtifactShow(ctx *orpheus.Context) error {
	if len(ctx.Positional()) != 1 {
		return errors.New(ErrUsage, "ringil artifact show <name>")
	}
	store, err := c.artifacts()
	if err != nil {
		return err
	}
	scope, err := c.artifactScope()
	if err != nil {
		return err
	}
	name := ctx.Positional()[0]
	_, data, err := store.Get(ctx.Context(), scope, name, 0)
	if errors.HasCode(err, artifact.ErrNotFound) {
		_, data, err = store.Get(ctx.Context(), scope+"-work", name, 0)
	}
	if err != nil {
		return err
	}
	if !ctx.GetFlagBool("raw") {
		// Model-written bytes: escape sequences could rewrite the terminal.
		data = []byte(clean(string(data)))
	}
	_, err = c.out.Write(data)
	return err
}

func (c *cli) cmdArtifactRemove(ctx *orpheus.Context) error {
	store, err := c.artifacts()
	if err != nil {
		return err
	}
	scope, err := c.artifactScope()
	if err != nil {
		return err
	}
	for _, name := range ctx.Positional() {
		if err := store.Delete(ctx.Context(), scope, name); err != nil {
			return err
		}
	}
	return nil
}

// stateSources are the parts of the agent's state a backup holds.
// Installed skills and sign-in tokens are left out: skills are reinstalled
// from their source, and tokens are credentials.
func (c *cli) stateSources() (map[string]string, error) {
	base, err := c.configDir()
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "no user config directory")
	}
	root := filepath.Join(base, "ringil")
	sources := map[string]string{}
	for _, n := range []string{"sessions", "memory", "projects", "workflows", "artifacts", "scratch"} {
		sources[n] = filepath.Join(root, n)
	}
	return sources, nil
}

func (c *cli) cmdStateBackup(ctx *orpheus.Context) error {
	if len(ctx.Positional()) != 1 {
		return errors.New(ErrUsage, "ringil state backup <file.tar.gz>")
	}
	sources, err := c.stateSources()
	if err != nil {
		return err
	}
	path := ctx.Positional()[0]
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the user names the file; never overwritten
	if err != nil {
		return errors.Wrap(err, ErrUsage, "cannot create "+path+" (it must not exist)")
	}
	m, err := backup.Create(ctx.Context(), f, sources, nil)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if rerr := os.Remove(path); rerr != nil {
			return errors.Wrap(err, ErrUsage, "backup failed and left "+path)
		}
		return err
	}
	return c.line("saved " + strconv.Itoa(len(m.Files)) + " files to " + clean(path))
}

func (c *cli) cmdStateVerify(ctx *orpheus.Context) error {
	if len(ctx.Positional()) != 1 {
		return errors.New(ErrUsage, "ringil state verify <file.tar.gz>")
	}
	f, err := os.Open(ctx.Positional()[0]) // #nosec G304 -- the user names the file
	if err != nil {
		return errors.Wrap(err, ErrUsage, "cannot open the backup")
	}
	defer closeFile(f)
	m, err := backup.Verify(ctx.Context(), f)
	if err != nil {
		return err
	}
	return c.line("ok: " + strconv.Itoa(len(m.Files)) + " files, made " + m.Created.Format("2006-01-02 15:04"))
}

func (c *cli) cmdStateRestore(ctx *orpheus.Context) error {
	if len(ctx.Positional()) != 2 {
		return errors.New(ErrUsage, "ringil state restore <file.tar.gz> <new directory>")
	}
	f, err := os.Open(ctx.Positional()[0]) // #nosec G304 -- the user names the file
	if err != nil {
		return errors.Wrap(err, ErrUsage, "cannot open the backup")
	}
	defer closeFile(f)
	m, err := backup.Restore(ctx.Context(), io.Reader(f), ctx.Positional()[1])
	if err != nil {
		return err
	}
	return c.line("restored " + strconv.Itoa(len(m.Files)) + " files into " + clean(ctx.Positional()[1]) + ", in quarantine: read " + backup.QuarantineFile + " there before using any of it")
}

func (c *cli) cmdStateHelp(*orpheus.Context) error {
	return c.line("ringil state backup <file> | verify <file> | restore <file> <new dir>")
}
