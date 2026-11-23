// network_linux.go: no network but a proxy, through a socat bridge
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
	"strings"
	"time"

	"github.com/agilira/go-errors"
	"golang.org/x/sys/unix"
)

// netStartTimeout bounds how long the user-mode network may take to come up;
// generous, because a loaded machine (a CI runner) can be slow.
const netStartTimeout = 30 * time.Second

// networkArgs prepares the descriptors for a proxied network: bwrap
// reports the sandbox's PID on an info pipe and waits on a block pipe
// until the bridge to the proxy is up, so the program never starts without
// it.
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
	// No DNS: clients hand the proxy names, not addresses.
	text, at := hostsFile, "/etc/hosts"
	data, err := dataFile(text)
	if err != nil {
		l.extra = append(l.extra, infoW, blockR)
		return nil, err
	}
	return []string{
		"--info-fd", l.add(infoW), "--block-fd", l.add(blockR),
		"--ro-bind-data", l.add(data), at,
	}, nil
}

const hostsFile = "127.0.0.1 localhost\n::1 localhost\n"

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

// connect attaches the network to the started sandbox and releases it.
func (l *launch) connect(ctx context.Context, p *process, spec Spec) error {
	infoR, blockW := l.extra[0], l.extra[1]
	pid, err := childPID(infoR)
	if err != nil {
		return err
	}
	return l.bridge(ctx, p, pid, spec.Proxy, blockW)
}

// nsGetUserns is NS_GET_USERNS, _IO(0xb7, 0x1): asked of a namespace, the
// kernel returns the user namespace that owns it.
const nsGetUserns = 0xb701

// namespaces opens the sandbox's network namespace and the user namespace
// that owns it. Asking the kernel for the owner, instead of reading
// /proc/PID/ns/user, matters: bwrap nests a second user namespace for
// --disable-userns shortly after reporting the PID, and a bridge joining
// that one could not reach the network. Holding both as descriptors
// also rules out PID reuse.
func namespaces(pid int) (*os.File, *os.File, error) {
	netns, err := os.Open("/proc/" + strconv.Itoa(pid) + "/ns/net")
	if err != nil {
		return nil, nil, errors.Wrap(err, ErrStart, "cannot open the sandbox's network namespace")
	}
	fd, err := unix.IoctlRetInt(int(netns.Fd()), nsGetUserns)
	if err != nil {
		return nil, nil, closeWith(errors.Wrap(err, ErrStart, "cannot find the network namespace's owner"), netns)
	}
	return netns, os.NewFile(uintptr(fd), "userns"), nil
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

// release lets bwrap run the program.
func release(block *os.File) error {
	if _, err := block.Write([]byte{1}); err != nil {
		return errors.Wrap(err, ErrStart, "cannot release the sandbox")
	}
	return nil
}

// bridge starts socat inside the sandbox's network namespace, but in the
// host's filesystem: it listens on ProxyAddr there and carries each
// connection to the proxy's Unix socket. The sandboxed program needs no
// Unix socket of its own, and its namespace has nothing else to reach.
func (l *launch) bridge(ctx context.Context, p *process, pid int, proxy string, blockW *os.File) error {
	netns, userns, err := namespaces(pid)
	if err != nil {
		return err
	}
	l.extra = append(l.extra, netns, userns)
	logR, logW, err := os.Pipe()
	if err != nil {
		return errors.Wrap(err, ErrStart, "cannot create the bridge's log pipe")
	}
	l.extra = append(l.extra, logR)
	// #nosec G204 -- fixed binaries; the socket path is the host's own
	cmd := exec.CommandContext(ctx, nsenterPath, "--preserve-credentials",
		"--user=/proc/self/fd/3", "--net=/proc/self/fd/4", "--",
		socatPath, "-d", "-d", "TCP-LISTEN:"+proxyPort()+",bind=127.0.0.1,fork,reuseaddr",
		"UNIX-CONNECT:"+proxy)
	cmd.ExtraFiles = []*os.File{userns, netns}
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Stderr = logW
	p.net = cmd
	err = cmd.Start()
	// The child has its copy: ours goes now, so that a bridge dying early
	// ends the wait at once.
	if cerr := logW.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		p.net = nil
		return errors.Wrap(err, ErrStart, "cannot start the proxy bridge")
	}
	if err := waitListening(logR); err != nil {
		return err
	}
	go drain(logR) // socat logs every connection: keep its pipe flowing
	return release(blockW)
}

func proxyPort() string { return ProxyAddr[strings.LastIndexByte(ProxyAddr, ':')+1:] }

// waitListening reads socat's log until it listens.
func waitListening(log *os.File) error {
	if err := log.SetReadDeadline(time.Now().Add(netStartTimeout)); err != nil {
		return errors.Wrap(err, ErrStart, "cannot wait on the proxy bridge")
	}
	sc := bufio.NewScanner(log)
	var last string
	for sc.Scan() {
		last = sc.Text()
		if strings.Contains(last, " listening on ") {
			return log.SetReadDeadline(time.Time{})
		}
	}
	return errors.New(ErrStart, "the proxy bridge did not come up").WithContext("socat", last)
}

func drain(r *os.File) {
	buf := make([]byte, 4096)
	for {
		if _, err := r.Read(buf); err != nil {
			return
		}
	}
}
