// sandbox.go: running a command in the sandbox from the terminal
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	stderrors "errors"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/sandbox"
)

// cmdSandbox runs a command the way skills' scripts, MCP servers and the
// Bash tool will run: useful to see what a program can and cannot do there.
func (c *cli) cmdSandbox(ctx *orpheus.Context) error {
	argv := ctx.Positional()
	if len(argv) == 0 {
		return errors.New(ErrUsage, "usage: ringil sandbox [--mount DIR[:/dest][:rw]]... [--net] -- command [args...]")
	}
	mounts, err := parseMounts(ctx.GetFlagStringSlice("mount"))
	if err != nil {
		return err
	}
	sb, err := c.newSandbox()
	if err != nil {
		return err
	}
	spec := sandbox.Spec{Argv: argv, Mounts: mounts, Stdin: c.in, Stdout: c.out, Stderr: c.errOut}
	if ctx.GetFlagBool("net") {
		spec.Network = sandbox.IsolatedNetwork
	}
	if dir := ctx.GetFlagString("dir"); dir != "" {
		spec.Dir = dir
	}
	if base, err := c.configDir(); err == nil {
		// The CLI's own sessions and configuration are never granted.
		spec.Protected = []string{filepath.Join(base, "ringil")}
	}
	p, err := sb.Start(ctx.Context(), spec)
	if err != nil {
		return err
	}
	return exitStatus(p.Wait())
}

// parseMounts reads DIR, DIR:rw, DIR:/dest or DIR:/dest:rw. Without a
// destination the directory appears at its own path.
func parseMounts(specs []string) ([]sandbox.Mount, error) {
	var mounts []sandbox.Mount
	for _, s := range specs {
		m, err := parseMount(s)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, m)
	}
	return mounts, nil
}

func parseMount(s string) (sandbox.Mount, error) {
	parts := strings.Split(s, ":")
	writable := len(parts) > 1 && parts[len(parts)-1] == "rw"
	if writable {
		parts = parts[:len(parts)-1]
	}
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
		return sandbox.Mount{}, errors.New(ErrUsage, "--mount takes DIR[:/dest][:rw]")
	}
	src, err := filepath.Abs(parts[0])
	if err != nil {
		return sandbox.Mount{}, errors.Wrap(err, ErrUsage, "cannot resolve "+parts[0])
	}
	m := sandbox.Mount{Source: src, Writable: writable}
	if len(parts) == 2 {
		m.Target = parts[1]
	}
	return m, nil
}

// exitStatus reports a non-zero exit of the sandboxed command as such.
func exitStatus(err error) error {
	var exit *exec.ExitError
	if stderrors.As(err, &exit) {
		return errors.New(ErrUsage, "command exited with status "+exit.String())
	}
	return err
}
