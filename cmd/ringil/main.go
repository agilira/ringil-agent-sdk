// main.go: entry point of the ringil command
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Command ringil runs an agent built on ringil-agent-sdk from the terminal:
// an interactive chat, or a single prompt with "ringil run".
//
// The model is any OpenAI-compatible endpoint, configured with flags or with
// RINGIL_BASE_URL, RINGIL_MODEL and RINGIL_API_KEY.
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
	code := run(ctx, os.Args[1:], newCLI(os.Stdin, os.Stdout, os.Stderr))
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, c *cli) int {
	if err := c.app().RunContext(ctx, args); err != nil {
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
