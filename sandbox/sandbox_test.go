// sandbox_test.go: checks of a Spec that need no sandbox
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package sandbox

import (
	"strconv"
	"testing"

	"github.com/agilira/go-errors"
)

// Audit 18/11: hidden paths over several mounts exceeded bubblewrap's
// argument limit and made every start fail obscurely.
func TestHiddenBudgetAcrossMounts(t *testing.T) {
	hide := make([]string, MaxHidden/2+1)
	for i := range hide {
		hide[i] = "d" + strconv.Itoa(i)
	}
	mounts := []Mount{{Source: "/a", Target: "/a", Hide: hide}, {Source: "/b", Target: "/b", Hide: hide}}
	if err := validateMounts(mounts); !errors.HasCode(err, ErrConfig) {
		t.Fatalf("budget not enforced: %v", err)
	}
	if err := validateMounts(mounts[:1]); err != nil {
		t.Fatalf("one mount within budget refused: %v", err)
	}
}
