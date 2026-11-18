// group_unix.go: stopping a host command with everything it started
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build !windows

package bash

import (
	"os/exec"
	"syscall"
)

// group puts the command in its own process group and, on cancel, kills
// the group: a command's background children do not outlive it.
func group(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
