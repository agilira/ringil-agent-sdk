// audit_schedule_test.go: regressions of the schedule audit (18/11)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/agilira/ringil-agent-sdk/trigger"
)

// jobs.json is hand-editable and the scheduler keeps invalid jobs listed:
// the listing prints When, Profile, Name and the zone error raw.
func TestAuditDescribeJobPrintsControlCharacters(t *testing.T) {
	j := trigger.Trigger{Name: "x\x1b]0;pwn\a", When: "\x1b[2J0 8 * * *", Profile: "p\x1b[31m",
		TimeZone: "\x1b[1;1H", Prompt: "hi"}
	out := describeTrigger(j, trigger.Status{}, time.Now())
	if strings.ContainsAny(out, "\x1b\a") {
		t.Fatalf("control characters reach the terminal: %q", out)
	}
}
