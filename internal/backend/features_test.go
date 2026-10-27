// -------------------------------------------------------------------------------
// Backend Tests - Unsupported Features
//
// Author: Alex Freidah
//
// Covers learning a feature is unsupported from the first refusal: native copy
// against an httptest fake S3 that refuses it, the gauge the first refusal
// sets, a failure that is not a refusal leaving the feature alone, the refusal
// classifier, and the set read through a circuit breaker.
// -------------------------------------------------------------------------------

package backend

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// copyServer answers every request with status and an S3 error named code,
// counting the requests it was sent.
func copyServer(status int, code string, requests *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`<Error><Code>` + code + `</Code><Message>no</Message></Error>`))
	}
}

// TestCopyObject_RefusalIsRemembered verifies a provider refusing server-side
// copy is reported as ErrCopyNotSupported, recorded with its gauge, and not
// asked again, and that the circuit breaker does not count the refusal.
func TestCopyObject_RefusalIsRemembered(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	be := newTestBackend(t, copyServer(http.StatusNotImplemented, "NotImplemented", &requests))

	for range 2 {
		if _, err := be.CopyObject(t.Context(), "src", "dst", "", nil); !errors.Is(err, ErrCopyNotSupported) {
			t.Errorf("err = %v, want ErrCopyNotSupported", err)
		}
	}
	if requests.Load() != 1 || !be.Unsupported().Has(core.FeatureNativeCopy) {
		t.Errorf("requests = %d, unsupported = %s; want one request and native-copy recorded",
			requests.Load(), be.Unsupported())
	}
	if got := testutil.ToFloat64(telemetry.BackendUnsupported.WithLabelValues(be.name, "native-copy")); got != 1 {
		t.Errorf("unsupported gauge = %v, want 1", got)
	}
	if classifyFailure(ErrCopyNotSupported) != scopeNone {
		t.Error("a refused copy counts against the breaker")
	}
}

// TestCopyObject_FailureIsNotARefusal verifies a copy that fails for another
// reason is reported as a failure and leaves the feature unrecorded, so the
// next copy is still tried natively.
func TestCopyObject_FailureIsNotARefusal(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	be := newTestBackend(t, copyServer(http.StatusInternalServerError, "InternalError", &requests))

	_, err := be.CopyObject(t.Context(), "src", "dst", "", nil)
	if err == nil || errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want a failure that is not ErrUnsupported", err)
	}
	if be.Unsupported() != 0 {
		t.Errorf("unsupported = %s, want none", be.Unsupported())
	}
}

// TestIsUnsupported verifies a refusal is recognized by HTTP status or by S3
// error code alone, and that a malformed request, which only batch delete
// treats as a refusal, is not.
func TestIsUnsupported(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"status not implemented", &httpError{code: http.StatusNotImplemented}, true},
		{"status method not allowed", &httpError{code: http.StatusMethodNotAllowed}, true},
		{"status bad request", &httpError{code: http.StatusBadRequest}, false},
		{"code not implemented", &smithy.GenericAPIError{Code: "NotImplemented"}, true},
		{"code access denied", &smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{"plain error", errors.New("connection reset"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isUnsupported(tc.err); got != tc.want {
				t.Errorf("isUnsupported(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestUnsupported_ThroughBreaker verifies the set reads through a circuit
// breaker, and that a backend that learns nothing reports none.
func TestUnsupported_ThroughBreaker(t *testing.T) {
	t.Parallel()
	be := newTestBackend(t, copyServer(http.StatusNotImplemented, "NotImplemented", new(atomic.Int32)))
	cb := NewCircuitBreakerBackend(be, CircuitBreakerConfig{Name: "test", Threshold: 1, Timeout: time.Minute})

	if _, err := cb.CopyObject(t.Context(), "src", "dst", "", nil); !errors.Is(err, ErrCopyNotSupported) {
		t.Fatalf("err = %v, want ErrCopyNotSupported", err)
	}
	if got := Unsupported(cb); got != core.FeatureNativeCopy {
		t.Errorf("Unsupported(breaker) = %s, want native-copy", got)
	}
	if !cb.Served().Has(core.CallCopyObject) {
		t.Error("the refused copy opened the CopyObject breaker")
	}
	if got := Unsupported(newMockBackend()); got != 0 {
		t.Errorf("Unsupported(mock) = %s, want none", got)
	}
}
