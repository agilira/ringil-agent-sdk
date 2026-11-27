// appworkflows_test.go: workflow runs and shutdown
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import "testing"

// A launch while shutdown waits does not race it, and does not start.
func TestWorkflowLaunchAfterWait(t *testing.T) {
	w := &wfRuns{running: map[string]uint64{}, failed: map[string]string{}}
	done := make(chan struct{})
	go func() { w.wait(); close(done) }()
	for range 50 {
		w.launch("a", func() error { return nil })
	}
	<-done
	ran := false
	w.launch("b", func() error { ran = true; return nil })
	w.wait()
	if ran {
		t.Fatal("a workflow started after shutdown")
	}
}
