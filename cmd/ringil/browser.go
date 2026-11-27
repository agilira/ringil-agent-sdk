// browser.go: opening a link in the user's browser
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/agilira/go-errors"
)

// openBrowser opens an http link of this machine in the default browser,
// by the system's own opener.
func openBrowser(link string) error {
	if !strings.HasPrefix(link, "http://127.0.0.1:") && !strings.HasPrefix(link, "http://[::1]:") {
		return errors.New(ErrUsage, "only a link of this machine is opened")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link) // #nosec G204 -- a loopback http link, checked above
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link) // #nosec G204 -- as above
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errors.New(ErrUsage, "no graphical session to open a browser in") // over SSH xdg-open would start a text browser
		}
		cmd = exec.Command("xdg-open", link) // #nosec G204 -- as above
	}
	if err := cmd.Start(); err != nil {
		return errors.Wrap(err, ErrUsage, "no browser could be opened")
	}
	go func() { _ = cmd.Wait() }() // the opener ends by itself; nothing is left behind
	return nil
}
