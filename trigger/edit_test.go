// edit_test.go: an edit judged against the store as it is
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import "testing"

// A pause that lands while an edit is being made holds; an edit keeps
// the trigger's name.
func TestEditKeepsAPauseMeanwhile(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Put(Trigger{Name: "daily", When: "0 8 * * *", Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	edited := Trigger{Name: "daily", When: "0 9 * * *", Prompt: "y"} // read before the pause
	if err := st.Pause("daily", true); err != nil {
		t.Fatal(err)
	}
	err = st.Edit("daily", func(old Trigger, found bool) (Trigger, error) {
		if !found {
			t.Fatal("not found")
		}
		edited.Paused = old.Paused
		return edited, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := st.Triggers()
	if len(list) != 1 || !list[0].Paused || list[0].When != "0 9 * * *" {
		t.Fatalf("%+v", list)
	}
	editKeepsTheName(t, st)
}

func editKeepsTheName(t *testing.T, st *Store) {
	t.Helper()
	if err := st.Edit("daily", func(old Trigger, _ bool) (Trigger, error) { old.Name = "other"; return old, nil }); err == nil {
		t.Fatal("an edit renamed the trigger")
	}
}
