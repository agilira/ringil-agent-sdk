// main.go: entry point of the ringil command
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Command ringil runs an agent built on ringil-agent-sdk from the terminal:
// an interactive chat, or a single prompt with "ringil run".
//
// The models are an ordered list of provider:model (models: in
// ringil.yaml, --models): the first answers, the others when it does not.
// A provider is named: a built-in one, or one of ringil.yaml's providers.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const (
	exitOK    = 0
	exitError = 1
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	c := newCLI(os.Stdin, os.Stdout, os.Stderr)
	c.ownProxy() // before any client is made: the proxy is ringil.yaml's, never the environment's
	code := run(ctx, os.Args[1:], c)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, c *cli) int {
	defer func() { _ = c.secrets.Close() }() // #nosec G104 -- the stores opened; nothing to report at exit
	defer c.flushTraces()
	err := c.app().RunContext(ctx, args)
	if c.pendingImport() { // serve stopped for an import from the app: done, it starts again
		c.flushTraces()
		_ = c.secrets.Close() // #nosec G104 -- as above
		c.restart()
		c.note("ringil could not start again by itself: start it again (ringil serve)")
	}
	if err != nil {
		return report(c.errOut, err)
	}
	return exitOK
}

func report(w io.Writer, err error) int {
	// A failed write has nowhere left to be reported; the exit code says it.
	if _, werr := fmt.Fprintln(w, "ringil: "+describe(err)); werr != nil {
		return exitError
	}
	return exitError
}
