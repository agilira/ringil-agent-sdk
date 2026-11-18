// audit_test.go: regressions of the egress audit (18/11)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package egress

import (
	"context"
	"io"
	"net"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

// One command's question about a domain holds p.asking; another command
// asking about a different domain waits for that lock without its
// context, so when its own run ends (timeout, cancel) its stop blocks
// until the first question is answered, however long the user takes.
func TestAuditStopWaitsForAnotherCommandsQuestion(t *testing.T) {
	p, err := New(Config{Ask: func(ctx context.Context, host string) (bool, error) {
		<-ctx.Done() // a well-behaved Ask: it returns when its context ends
		return false, ctx.Err()
	}, Resolver: names{}})
	if err != nil {
		t.Fatal(err)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	sockA, stopA, err := p.Listen(ctxA, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stopA()
	ctxB, cancelB := context.WithCancel(context.Background())
	sockB, stopB, err := p.Listen(ctxB, nil)
	if err != nil {
		t.Fatal(err)
	}
	send := func(sock, host string) {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_, _ = io.WriteString(c, "CONNECT "+host+":443 HTTP/1.1\r\nHost: "+host+":443\r\n\r\n")
	}
	send(sockA, "first.example.com")
	time.Sleep(100 * time.Millisecond)
	send(sockB, "second.example.com")
	time.Sleep(100 * time.Millisecond)
	cancelB() // B's command timed out
	stopped := make(chan struct{})
	go func() { stopB(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		var dump strings.Builder
		_ = pprof.Lookup("goroutine").WriteTo(&dump, 1)
		t.Errorf("stopping a finished command's proxy waits for another command's question; goroutines:\n%s", dump.String())
		cancelA()
		<-stopped
	}
}
