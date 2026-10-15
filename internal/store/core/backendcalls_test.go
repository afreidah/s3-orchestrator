// -------------------------------------------------------------------------------
// Backend Calls - Tests
//
// Author: Alex Freidah
//
// The call vocabulary, the required masks consumers test against, and the
// mapping from an operation to its call. The masks matter most: a write mask
// that did not require PutObject would let placement pick a backend refusing
// uploads.
// -------------------------------------------------------------------------------

package core

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// TestBackendCalls_BitsAreDistinct pins that every call occupies its own bit,
// that AllCalls carries each one, and that every name is a known operation.
func TestBackendCalls_BitsAreDistinct(t *testing.T) {
	t.Parallel()
	if err := backendCallNames.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, b := range backendCallNames {
		if !AllCalls.Has(b.Bit) {
			t.Errorf("AllCalls does not carry %s", b.Name)
		}
		if !s3op.Known(b.Name) {
			t.Errorf("%s is not an s3op operation", b.Name)
		}
	}
}

// TestBackendCalls_RequiredMasks pins what each purpose requires: a backend is
// refused for a purpose when any call it makes is missing, and a backend
// serving every call is accepted everywhere.
func TestBackendCalls_RequiredMasks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		required BackendCalls
		calls    []BackendCalls
	}{
		{"write", RequiredForWrite, []BackendCalls{CallPutObject, CallGetObject}},
		{"read", RequiredForRead, []BackendCalls{CallGetObject}},
		{"probe", RequiredForProbe, []BackendCalls{CallHeadObject}},
	} {
		if !AllCalls.Has(tc.required) {
			t.Errorf("%s: a backend serving every call is refused", tc.name)
		}
		for _, call := range tc.calls {
			if (AllCalls &^ call).Has(tc.required) {
				t.Errorf("%s: a backend without %s is accepted", tc.name, call)
			}
		}
	}
	if !(AllCalls &^ CallListObjects).Has(RequiredForWrite) {
		t.Error("a backend refusing listings is refused for writes")
	}
}

// TestCallFor maps each guarded operation to its bit and refuses one no
// breaker guards.
func TestCallFor(t *testing.T) {
	t.Parallel()
	if call, ok := CallFor(s3op.GetObject); !ok || call != CallGetObject {
		t.Errorf("CallFor(GetObject) = %s, %t", call, ok)
	}
	if _, ok := CallFor(s3op.UploadPart); ok {
		t.Error("CallFor(UploadPart) found a call")
	}
}

// TestBackendCalls_Render covers names, the joined form and JSON.
func TestBackendCalls_Render(t *testing.T) {
	t.Parallel()
	set := CallGetObject | CallPutObject
	if got := set.Names(); !slices.Equal(got, []string{"PutObject", "GetObject"}) {
		t.Errorf("Names = %v", got)
	}
	if got := set.String(); got != "PutObject,GetObject" {
		t.Errorf("String = %q", got)
	}
	got, err := json.Marshal(set)
	if err != nil || string(got) != `["PutObject","GetObject"]` {
		t.Errorf("json.Marshal = %s, %v", got, err)
	}
}
