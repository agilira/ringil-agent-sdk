// race_off_test.go: whether the race detector is on
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build !race

package trigger

const raceEnabled = false
