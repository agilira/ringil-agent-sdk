// state.go: artifacts in the terminal
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"path/filepath"
	"strconv"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/artifact"
)

// artifacts is the terminal's store: a scope per project. A run of serve
// (shared) works in the app's store instead, a scope per conversation:
// the two never meet.
func (c *cli) artifacts() (*artifact.Local, error) {
	if c.shared {
		return c.appArtifacts()
	}
	return c.artifactStore("artifacts")
}

// appArtifacts is the store of serve's conversations, which the app reads.
func (c *cli) appArtifacts() (*artifact.Local, error) { return c.artifactStore("app-artifacts") }

func (c *cli) artifactStore(name string) (*artifact.Local, error) {
	base, err := c.configDir()
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "no user config directory")
	}
	return artifact.NewLocal(filepath.Join(base, "ringil", name))
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

// artifactTools: in the terminal the agent works in the project's scope
// and reads what its workers made; a worker works in the workers' scope
// and reads the project's, so a worker can never replace what the agent
// reads as the project's latest. In serve a run's context gives its
// conversation's scope, the only one it and its workers see: what a
// worker makes there is the user's to see in the app, by "worker", and
// a change made on a version no longer the latest is refused.
func (c *cli) artifactTools(worker bool) []agent.Tool {
	store, err := c.artifacts()
	if err != nil {
		return nil
	}
	project, err := c.artifactScope()
	if err != nil {
		return nil
	}
	if c.shared { // serve: the run's conversation is its only scope, and without one there is none
		by := "agent"
		if worker {
			by = "worker"
		}
		return artifact.Tools(store, artifact.Access{By: by, Write: true, Files: c.madeFile})
	}
	work := project + "-work"
	if worker {
		return artifact.Tools(store, artifact.Access{Scope: work, By: "worker", Write: true, Read: []string{project}, Files: c.madeFile})
	}
	return artifact.Tools(store, artifact.Access{Scope: project, By: "agent", Write: true, Read: []string{work}, Files: c.madeFile})
}

// maxFileArtifact bounds a file the agent saves as an artifact: the
// store's own bound.
const maxFileArtifact = 64 << 20

// madeFile is a file the agent made, read as read_file reads: in serve a
// run's artifacts go to its conversation (artifact.WithScope), in the
// terminal to the project's scope.
func (c *cli) madeFile(ctx context.Context, path string) ([]byte, string, error) {
	if c.ws == nil {
		return nil, "", agent.ToolError(artifact.ErrInput, "No files are open to this agent.")
	}
	return c.ws.ReadFile(ctx, path, maxFileArtifact)
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
			if err := c.line(clean(a.Name) + " v" + strconv.Itoa(a.Version) + "  " + a.Kind + "  " + strconv.Quote(clean(a.Title)) + "  " + strconv.FormatInt(a.Size, 10) + " bytes  " + a.Created.Format("2006-01-02 15:04") + "  " + short(a.By) + where); err != nil {
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
