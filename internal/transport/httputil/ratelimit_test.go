// -------------------------------------------------------------------------------
// Rate Limiter Tests
//
// Author: Alex Freidah
//
// Tests for per-IP rate limiting middleware. Validates token bucket enforcement,
// burst allowance, the client the refusal names, and 429 responses.
// -------------------------------------------------------------------------------

package httputil

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit/audittest"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// newTestLimiter builds a limiter at rps and burst with the cleanup cadence the
// config loader fills by default.
func newTestLimiter(rps float64, burst int) *RateLimiter {
	return NewRateLimiter(config.RateLimitConfig{
		Enabled: true, RequestsPerSec: rps, Burst: burst, CleanupInterval: time.Minute, CleanupMaxAge: 5 * time.Minute,
	})
}

// TestRateLimiter_AllowAndBlock verifies a burst is allowed, the request past
// it is refused, and another IP keeps its own bucket.
func TestRateLimiter_AllowAndBlock(t *testing.T) {
	t.Parallel()
	rl := newTestLimiter(1, 2)
	defer rl.Close()

	for i := range 2 {
		if !rl.Allow("10.0.0.1") {
			t.Fatalf("request %d within the burst should be allowed", i+1)
		}
	}
	if rl.Allow("10.0.0.1") {
		t.Error("third request should be blocked (burst exhausted)")
	}
	if !rl.Allow("10.0.0.2") {
		t.Error("different IP should have its own bucket")
	}
}

// TestRateLimiter_RefusesInTheSurfacesFormat verifies a refusal answers 429
// with Retry-After through the surface's own error writer, counts the metric,
// and is audited once under that surface's route. Not parallel: it captures
// the global logger.
func TestRateLimiter_RefusesInTheSurfacesFormat(t *testing.T) {
	rl := newTestLimiter(1, 1)
	defer rl.Close()
	handler := rl.Middleware("admin", WriteJSONCodedError)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	before := testutil.ToFloat64(telemetry.RateLimitRejectionsTotal)

	var rec *httptest.ResponseRecorder
	entries := audittest.Capture(t, func() {
		for range 2 {
			rec = httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/status", nil)
			req.RemoteAddr = "10.0.0.7:12345"
			handler.ServeHTTP(rec, req)
		}
	})

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q, want 1", ra)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want the admin surface's JSON", ct)
	}
	if after := testutil.ToFloat64(telemetry.RateLimitRejectionsTotal); after <= before {
		t.Errorf("RateLimitRejectionsTotal did not increment: before=%v, after=%v", before, after)
	}
	if n := len(audittest.Named(entries, "admin.RateLimitRejected")); n != 1 {
		t.Errorf("admin.RateLimitRejected entries = %d, want 1", n)
	}
	if n := len(audittest.WithPrefix(entries, "s3.")); n != 0 {
		t.Errorf("an admin refusal was audited as an S3 event %d time(s)", n)
	}
}

// TestRateLimiter_RefusalNamesResolvedClient verifies the refusal's audit entry
// names the client resolved through a trusted proxy, not the proxy itself. Not
// parallel: it captures the global logger.
func TestRateLimiter_RefusalNamesResolvedClient(t *testing.T) {
	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled: true, RequestsPerSec: 1, Burst: 1, CleanupInterval: time.Minute, CleanupMaxAge: time.Minute,
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	defer rl.Close()
	handler := rl.Middleware("s3", WriteJSONCodedError)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	entries := audittest.Capture(t, func() {
		for range 2 {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/bucket/key", nil)
			req.RemoteAddr = "10.0.0.1:12345"
			req.Header.Set("X-Forwarded-For", "203.0.113.50")
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}
	})

	refused := audittest.Named(entries, "s3.RateLimitRejected")
	if len(refused) != 1 {
		t.Fatalf("s3.RateLimitRejected entries = %d, want 1", len(refused))
	}
	if got := refused[0]["client_addr"]; got != "203.0.113.50" {
		t.Errorf("client_addr = %v, want the resolved client 203.0.113.50", got)
	}
}

// TestRateLimiter_UpdateLimits_NewVisitors verifies a visitor first seen after
// an update gets the new burst.
func TestRateLimiter_UpdateLimits_NewVisitors(t *testing.T) {
	t.Parallel()
	rl := newTestLimiter(1, 1)
	defer rl.Close()

	rl.UpdateLimits(1000, 1000)

	for i := range 100 {
		if !rl.Allow("10.0.0.99") {
			t.Fatalf("request %d should be allowed with new burst=1000", i+1)
		}
	}
}

// TestRateLimiter_UpdateLimits_ClearsExistingVisitors verifies an update gives
// a visitor that had exhausted its burst a fresh bucket at the new limits.
func TestRateLimiter_UpdateLimits_ClearsExistingVisitors(t *testing.T) {
	t.Parallel()
	rl := newTestLimiter(1, 2)
	defer rl.Close()

	for i := range 2 {
		if !rl.Allow("10.0.0.1") {
			t.Fatalf("request %d within the burst should be allowed", i+1)
		}
	}

	rl.UpdateLimits(1, 1000)

	for i := range 100 {
		if !rl.Allow("10.0.0.1") {
			t.Fatalf("existing visitor request %d should be allowed with new burst=1000", i+1)
		}
	}
}

// TestRateLimiter_SweepsOnTheConfiguredInterval verifies the cleanup runs
// every cleanup_interval and drops an IP idle longer than cleanup_max_age: the
// sweep at 2s keeps it, the one at 4s removes it.
func TestRateLimiter_SweepsOnTheConfiguredInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rl := NewRateLimiter(config.RateLimitConfig{
			Enabled:         true,
			RequestsPerSec:  100,
			Burst:           100,
			CleanupInterval: 2 * time.Second,
			CleanupMaxAge:   3 * time.Second,
		})
		defer rl.Close()
		rl.Allow("10.0.0.1")

		for _, step := range []struct {
			after time.Duration
			want  int
		}{{2 * time.Second, 1}, {2 * time.Second, 0}} {
			time.Sleep(step.after)
			synctest.Wait()
			rl.visitors.mu.Lock()
			got := len(rl.visitors.entries)
			rl.visitors.mu.Unlock()
			if got != step.want {
				t.Fatalf("visitors = %d, want %d", got, step.want)
			}
		}
	})
}
