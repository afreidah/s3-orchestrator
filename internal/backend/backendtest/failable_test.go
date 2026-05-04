// -------------------------------------------------------------------------------
// FailableBackend Tests
//
// Author: Alex Freidah
//
// Exercises the toggle surface of FailableBackend against a mock inner backend
// so tests adopting the wrapper can rely on its basic contracts (pass-through
// when healthy, error injection when toggled, per-method overrides, ability
// to clear failures).
// -------------------------------------------------------------------------------

package backendtest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"
)

// newPassThroughBackend builds a MockObjectBackend with EXPECT calls configured
// to pass through successfully (returning empty but valid results) so tests
// can assert failure injection against a healthy inner backend.
func newPassThroughBackend(t *testing.T) *MockObjectBackend {
	t.Helper()
	ctrl := gomock.NewController(t)
	m := NewMockObjectBackend(ctrl)
	m.EXPECT().PutObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return("etag", nil).AnyTimes()
	m.EXPECT().GetObject(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	m.EXPECT().HeadObject(gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	m.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		Return(nil).AnyTimes()
	return m
}

// TestFailableBackend_PassthroughWhenHealthy verifies that methods delegate
// to the inner backend unchanged when no failure is configured.
func TestFailableBackend_PassthroughWhenHealthy(t *testing.T) {
	t.Parallel()
	fb := New(newPassThroughBackend(t))

	etag, err := fb.PutObject(context.Background(), "k", bytes.NewReader([]byte("v")), 1, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if etag != "etag" {
		t.Fatalf("etag = %q, want %q", etag, "etag")
	}
	if err := fb.DeleteObject(context.Background(), "k"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
}

// TestFailableBackend_SetFailing_AllMethodsFail verifies the failable backend set failing all methods fail contract.
// Asserts that PutObject err = , want.
func TestFailableBackend_SetFailing_AllMethodsFail(t *testing.T) {
	t.Parallel()
	fb := New(newPassThroughBackend(t))

	injected := errors.New("db down")
	fb.SetFailing(true, injected)

	if _, err := fb.PutObject(context.Background(), "k", bytes.NewReader([]byte("v")), 1, "", nil); !errors.Is(err, injected) {
		t.Errorf("PutObject err = %v, want %v", err, injected)
	}
	if _, err := fb.GetObject(context.Background(), "k", ""); !errors.Is(err, injected) {
		t.Errorf("GetObject err = %v, want %v", err, injected)
	}
	if _, err := fb.HeadObject(context.Background(), "k"); !errors.Is(err, injected) {
		t.Errorf("HeadObject err = %v, want %v", err, injected)
	}
	if err := fb.DeleteObject(context.Background(), "k"); !errors.Is(err, injected) {
		t.Errorf("DeleteObject err = %v, want %v", err, injected)
	}
}

// TestFailableBackend_SetFailing_NilUsesDefault verifies that passing nil as
// the error argument falls back to the package default sentinel.
func TestFailableBackend_SetFailing_NilUsesDefault(t *testing.T) {
	t.Parallel()
	fb := New(newPassThroughBackend(t))
	fb.SetFailing(true, nil)

	_, err := fb.PutObject(context.Background(), "k", bytes.NewReader([]byte("v")), 1, "", nil)
	if !errors.Is(err, ErrSimulatedBackendFailure) {
		t.Errorf("err = %v, want ErrSimulatedBackendFailure", err)
	}
}

// TestFailableBackend_SetErr_PerMethodOverride verifies that per-method errors
// affect only the targeted method and leave others passing through.
func TestFailableBackend_SetErr_PerMethodOverride(t *testing.T) {
	t.Parallel()
	fb := New(newPassThroughBackend(t))
	onlyGet := errors.New("get-only")
	fb.SetErr(MethodGet, onlyGet)

	if _, err := fb.GetObject(context.Background(), "k", ""); !errors.Is(err, onlyGet) {
		t.Errorf("GetObject err = %v, want %v", err, onlyGet)
	}
	if _, err := fb.PutObject(context.Background(), "k", bytes.NewReader([]byte("v")), 1, "", nil); err != nil {
		t.Errorf("PutObject should pass through; got %v", err)
	}
}

// TestFailableBackend_Clear verifies the failable backend clear contract.
// Asserts that GetObject after clear:.
func TestFailableBackend_Clear(t *testing.T) {
	t.Parallel()
	fb := New(newPassThroughBackend(t))

	fb.SetFailing(true, nil)
	fb.SetErr(MethodGet, errors.New("transient"))
	fb.SetFailing(false, nil) // should also clear per-method overrides

	if _, err := fb.GetObject(context.Background(), "k", ""); err != nil {
		t.Fatalf("GetObject after clear: %v", err)
	}
	if _, err := fb.PutObject(context.Background(), "k", bytes.NewReader([]byte("v")), 1, "", nil); err != nil {
		t.Fatalf("PutObject after clear: %v", err)
	}
}

// TestFailableBackend_SetErr_Nil_ClearsOne verifies clearing a single method
// without disturbing other configured failures.
func TestFailableBackend_SetErr_Nil_ClearsOne(t *testing.T) {
	t.Parallel()
	fb := New(newPassThroughBackend(t))
	fb.SetErr(MethodGet, errors.New("g"))
	fb.SetErr(MethodPut, errors.New("p"))

	fb.SetErr(MethodGet, nil) // clear only the Get override

	if _, err := fb.GetObject(context.Background(), "k", ""); err != nil {
		t.Errorf("GetObject should pass through after clearing: got %v", err)
	}
	if _, err := fb.PutObject(context.Background(), "k", bytes.NewReader([]byte("v")), 1, "", nil); err == nil {
		t.Errorf("PutObject should still fail; got nil")
	}
}
