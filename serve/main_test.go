// main_test.go: the tests keep the program's places in a temporary home
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve_test

import (
	"testing"

	"github.com/agilira/ringil-agent-sdk/internal/testhome"
)

func TestMain(m *testing.M) { testhome.Main(m) }
