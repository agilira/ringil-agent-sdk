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

	"github.com/agilira/ringil-agent-sdk/egress"
	"github.com/agilira/ringil-agent-sdk/sandbox"
)

// cmdSandbox runs a command the way skills' scripts, MCP servers and the
// Bash tool will run: useful to see what a program can and cannot do there.
func (c *cli) cmdSandbox(ctx *orpheus.Context) error {
	argv := ctx.Positional()
	if len(argv) == 0 {
		return errors.New(ErrUsage, "usage: ringil sandbox [--mount DIR[:/dest][:rw]]... [--allow-domain D]... -- command [args...]")
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
	if domains := ctx.GetFlagStringSlice("allow-domain"); len(domains) > 0 {
		stop, err := proxied(ctx, &spec, domains)
		if err != nil {
			return err
		}
		defer stop()
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

// proxied gives spec the network to domains alone, through the egress
// proxy, as bash and MCP servers have it.
func proxied(ctx *orpheus.Context, spec *sandbox.Spec, domains []string) (func(), error) {
	p, err := egress.New(egress.Config{Allow: domains})
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "--allow-domain: "+err.Error())
	}
	sock, stop, err := p.Listen(ctx.Context(), nil)
	if err != nil {
		return nil, err
	}
	spec.Network, spec.Proxy = sandbox.ProxiedNetwork, sock
	return stop, nil
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
	src, writable := strings.CutSuffix(s, ":rw")
	// The destination is an absolute sandbox path after the last ":/";
	// splitting on every ":" would break Windows sources such as C:\data.
	dest := ""
	if i := strings.LastIndex(src, ":/"); i >= 0 {
		src, dest = src[:i], src[i+1:]
	}
	if src == "" || strings.Contains(dest, ":") {
		return sandbox.Mount{}, errors.New(ErrUsage, "--mount takes DIR[:/dest][:rw]")
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return sandbox.Mount{}, errors.Wrap(err, ErrUsage, "cannot resolve "+src)
	}
	return sandbox.Mount{Source: abs, Target: dest, Writable: writable}, nil
}

// exitStatus reports a non-zero exit of the sandboxed command as such.
func exitStatus(err error) error {
	var exit *exec.ExitError
	if stderrors.As(err, &exit) {
		return errors.New(ErrUsage, "command exited with status "+exit.String())
	}
	return err
}
