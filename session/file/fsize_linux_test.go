// fsize_linux_test.go: a write failure in the middle of an Append
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package file

import (
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Findings 3 and 4: after a failed write the session refuses further
// Appends, and the partly written tail is removed.
func TestFailedWriteFreezesSession(t *testing.T) {
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	s, dir := newStore(t)
	sess := open(t, s, "w")
	appendOK(t, sess, agent.UserText("hi"))
	info, err := os.Stat(filepath.Join(dir, "w.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	limit := syscall.Rlimit{Cur: uint64(info.Size()) + 300, Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	failed := sess.Append(bg, agent.AssistantText("short"), agent.UserText(strings.Repeat("A", 4000)))
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if !errors.HasCode(failed, agent.ErrSessionIO) {
		t.Fatalf("oversized append %v", failed)
	}
	if err := sess.Append(bg, agent.AssistantText("ok")); !errors.HasCode(err, agent.ErrSessionIO) {
		t.Fatalf("append after a failed write: %v", err)
	}
	closeOK(t, sess)
	again := open(t, s, "w")
	defer closeOK(t, again)
	if got := again.Messages(); len(got) != 1 || got[0].Text() != "hi" {
		t.Fatalf("after reopen %+v", got)
	}
}
