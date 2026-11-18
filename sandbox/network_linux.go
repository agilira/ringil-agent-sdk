// network_linux.go: an isolated network through slirp4netns
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/agilira/go-errors"
)

// netStartTimeout bounds how long the user-mode network may take to come up.
const netStartTimeout = 10 * time.Second

// resolvConf points DNS at slirp4netns' forwarder.
const resolvConf = "nameserver 10.0.2.3\n"

// networkArgs prepares the descriptors for an isolated network: bwrap
// reports the sandbox's PID on an info pipe and waits on a block pipe
// until slirp4netns has attached the network, so the program never starts
// without it.
func (l *launch) networkArgs(n Network) ([]string, error) {
	if n == NoNetwork {
		return nil, nil
	}
	infoR, infoW, err := os.Pipe()
	if err != nil {
		return nil, errors.Wrap(err, ErrStart, "cannot create the info pipe")
	}
	// The host ends come first in extra: connect finds them there.
	l.extra = append(l.extra, infoR)
	blockR, blockW, err := os.Pipe()
	if err != nil {
		l.extra = append(l.extra, infoW)
		return nil, errors.Wrap(err, ErrStart, "cannot create the block pipe")
	}
	l.extra = append(l.extra, blockW)
	resolv, err := dataFile(resolvConf)
	if err != nil {
		l.extra = append(l.extra, infoW, blockR)
		return nil, err
	}
	return []string{
		"--info-fd", l.add(infoW), "--block-fd", l.add(blockR),
		"--ro-bind-data", l.add(resolv), "/etc/resolv.conf",
		"--ro-bind-try", "/etc/ssl", "/etc/ssl",
		"--ro-bind-try", "/etc/ca-certificates", "/etc/ca-certificates",
	}, nil
}

// dataFile returns an unlinked file holding text, for --ro-bind-data.
func dataFile(text string) (*os.File, error) {
	f, err := os.CreateTemp("", "ringil-sandbox-")
	if err != nil {
		return nil, errors.Wrap(err, ErrStart, "cannot create a sandbox data file")
	}
	if err := os.Remove(f.Name()); err != nil {
		return nil, closeWith(errors.Wrap(err, ErrStart, "cannot unlink a sandbox data file"), f)
	}
	if _, err := f.WriteString(text); err != nil {
		return nil, closeWith(errors.Wrap(err, ErrStart, "cannot write a sandbox data file"), f)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, closeWith(errors.Wrap(err, ErrStart, "cannot rewind a sandbox data file"), f)
	}
	return f, nil
}

// closeWith closes f after a failure, keeping the failure's code.
func closeWith(err *errors.Error, f *os.File) error {
	if cerr := f.Close(); cerr != nil {
		return err.WithContext("close_error", cerr.Error())
	}
	return err
}

// connect attaches slirp4netns to the started sandbox and releases it.
func (l *launch) connect(ctx context.Context, p *process) error {
	infoR, blockW := l.extra[0], l.extra[1]
	pid, err := childPID(infoR)
	if err != nil {
		return err
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return errors.Wrap(err, ErrStart, "cannot create the ready pipe")
	}
	exitR, exitW, err := os.Pipe()
	if err != nil {
		l.extra = append(l.extra, readyR, readyW)
		return errors.Wrap(err, ErrStart, "cannot create the exit pipe")
	}
	// exitW stays open on our side for the sandbox's life: when Wait
	// closes it, slirp4netns exits.
	l.extra = append(l.extra, readyR, exitW)
	p.net = slirpCommand(ctx, pid, readyW, exitR)
	err = p.net.Start()
	l.extra = append(l.extra, readyW, exitR) // the child has its copies now
	if err != nil {
		p.net = nil
		return errors.Wrap(err, ErrStart, "cannot start slirp4netns")
	}
	if err := waitReady(readyR); err != nil {
		return err
	}
	return release(blockW)
}

func slirpCommand(ctx context.Context, pid int, ready, exit *os.File) *exec.Cmd {
	// #nosec G204 -- fixed binary and arguments built from a PID
	cmd := exec.CommandContext(ctx, slirpPath, "--configure", "--mtu=65520", "--disable-host-loopback",
		"--userns-path=/proc/"+strconv.Itoa(pid)+"/ns/user",
		"--ready-fd=3", "--exit-fd=4", strconv.Itoa(pid), "tap0")
	cmd.ExtraFiles = []*os.File{ready, exit}
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	return cmd
}

// childPID reads the sandbox's PID from bwrap's info descriptor.
func childPID(info *os.File) (int, error) {
	if err := info.SetReadDeadline(time.Now().Add(netStartTimeout)); err != nil {
		return 0, errors.Wrap(err, ErrStart, "cannot wait on bwrap")
	}
	var status struct {
		ChildPID int `json:"child-pid"`
	}
	if err := json.NewDecoder(bufio.NewReader(info)).Decode(&status); err != nil || status.ChildPID <= 0 {
		return 0, errors.New(ErrStart, "bwrap did not report the sandbox PID")
	}
	return status.ChildPID, nil
}

func waitReady(ready *os.File) error {
	if err := ready.SetReadDeadline(time.Now().Add(netStartTimeout)); err != nil {
		return errors.Wrap(err, ErrStart, "cannot wait on slirp4netns")
	}
	buf := make([]byte, 1)
	if _, err := ready.Read(buf); err != nil {
		return errors.Wrap(err, ErrStart, "the isolated network did not come up")
	}
	return nil
}

// release lets bwrap run the program.
func release(block *os.File) error {
	if _, err := block.Write([]byte{1}); err != nil {
		return errors.Wrap(err, ErrStart, "cannot release the sandbox")
	}
	return nil
}
