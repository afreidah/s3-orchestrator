// -------------------------------------------------------------------------------
// Backend State Flags - Tests
//
// Author: Alex Freidah
//
// The flag vocabulary, the accept masks consumers test against, and the
// drain-progress name a set reports. The masks matter most: a write mask that
// accepted a flag would let placement pick a backend the operator took out of
// service.
// -------------------------------------------------------------------------------

package core

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestBackendFlags_BitsAreDistinct pins that every flag occupies its own bit.
func TestBackendFlags_BitsAreDistinct(t *testing.T) {
	t.Parallel()
	if err := backendFlagNames.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestBackendFlags_AcceptMasks pins what each purpose accepts: writes take no
// flagged backend, reconcile takes a cordoned one but no drain, and a backend
// with no flags is accepted everywhere.
func TestBackendFlags_AcceptMasks(t *testing.T) {
	t.Parallel()
	for _, f := range []BackendFlags{FlagCordoned, FlagDraining, FlagDrained, FlagDrainFailed} {
		if f.AcceptedBy(AcceptForWrite) {
			t.Errorf("a %s backend is accepted for writes", f)
		}
	}
	if !FlagCordoned.AcceptedBy(AcceptForReconcile) {
		t.Error("a cordoned backend is not accepted for reconcile")
	}
	if (FlagCordoned | FlagDraining).AcceptedBy(AcceptForReconcile) {
		t.Error("a draining backend is accepted for reconcile")
	}
	if !BackendFlags(0).AcceptedBy(AcceptForWrite) {
		t.Error("a backend with no flags is held back")
	}
}

// TestBackendFlags_MarshalJSON pins the JSON form to the flag names.
func TestBackendFlags_MarshalJSON(t *testing.T) {
	t.Parallel()
	for flags, want := range map[BackendFlags]string{
		0:                           `[]`,
		FlagCordoned:                `["cordoned"]`,
		FlagCordoned | FlagDraining: `["cordoned","draining"]`,
	} {
		got, err := json.Marshal(flags)
		if err != nil || string(got) != want {
			t.Errorf("json.Marshal(%d) = %s, %v; want %s", int64(flags), got, err, want)
		}
	}
}

// TestBackendFlags_Render covers names, the joined form and the drain-progress
// name, including a cordon alongside a drain.
func TestBackendFlags_Render(t *testing.T) {
	t.Parallel()
	both := FlagCordoned | FlagDraining
	if got := both.Names(); !slices.Equal(got, []string{"cordoned", "draining"}) {
		t.Errorf("Names = %v", got)
	}
	if got := both.String(); got != "cordoned,draining" {
		t.Errorf("String = %q", got)
	}
	for flags, want := range map[BackendFlags]string{
		0:                              "",
		FlagCordoned:                   "",
		both:                           "draining",
		FlagDrained:                    "drained",
		FlagCordoned | FlagDrainFailed: "failed",
	} {
		if got := flags.DrainState(); got != want {
			t.Errorf("%s.DrainState() = %q, want %q", flags, got, want)
		}
	}
}
