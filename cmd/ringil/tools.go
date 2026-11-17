// tools.go: tools offered by default, harmless by construction
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"math"
	"strconv"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
)

// clock is replaced in tests.
var clock = time.Now

type nowIn struct {
	Zone string `json:"zone,omitempty" desc:"IANA time zone, e.g. Europe/Rome; default: local"`
}

type calcIn struct {
	A  float64 `json:"a"`
	Op string  `json:"op" enum:"+,-,*,/" desc:"Operation"`
	B  float64 `json:"b"`
}

// builtinTools are read-only and touch nothing outside the process: enough
// to see the loop work without granting the model anything.
func builtinTools() (agent.Registry, error) {
	return agent.NewRegistry(
		agent.NewTool("now", "Current date and time", now, agent.ReadOnly()),
		agent.NewTool("calculate", "Apply one arithmetic operation to two numbers", calculate, agent.ReadOnly()),
	)
}

func now(_ context.Context, in nowIn) (string, error) {
	loc := time.Local
	if in.Zone != "" {
		var err error
		if loc, err = time.LoadLocation(in.Zone); err != nil {
			return "", agent.ToolError("UNKNOWN_ZONE", "Unknown time zone; use an IANA name such as Europe/Rome.")
		}
	}
	return clock().In(loc).Format("Monday 2 January 2006, 15:04:05 MST"), nil
}

func calculate(_ context.Context, in calcIn) (string, error) {
	var r float64
	switch in.Op {
	case "+":
		r = in.A + in.B
	case "-":
		r = in.A - in.B
	case "*":
		r = in.A * in.B
	case "/":
		if in.B == 0 {
			return "", agent.ToolError("DIVISION_BY_ZERO", "Division by zero.")
		}
		r = in.A / in.B
	}
	if math.IsInf(r, 0) || math.IsNaN(r) {
		return "", agent.ToolError("OVERFLOW", "The result is not a finite number.")
	}
	return strconv.FormatFloat(r, 'g', -1, 64), nil
}
