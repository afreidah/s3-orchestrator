// -------------------------------------------------------------------------------
// Rate Limiter Tests
//
// Author: Alex Freidah
//
// Tests for per-IP rate limiting middleware. Validates token bucket enforcement,
// burst allowance, trusted proxy header extraction, and 429 responses.
// -------------------------------------------------------------------------------

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRateLimiter_AllowAndBlock(t *testing.T) {
	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1,
		Burst:          2,
	})

	// First 2 requests (burst) should be allowed
	if !rl.Allow("10.0.0.1") {
		t.Error("first request should be allowed")
	}
	if !rl.Allow("10.0.0.1") {
		t.Error("second request (within burst) should be allowed")
	}

	// Third request should be blocked (burst exhausted, rate is 1/s)
	if rl.Allow("10.0.0.1") {
		t.Error("third request should be blocked (burst exhausted)")
	}

	// Different IP should still be allowed
	if !rl.Allow("10.0.0.2") {
		t.Error("different IP should have its own bucket")
	}
}

func TestRateLimiter_Middleware429(t *testing.T) {
	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1,
		Burst:          1,
	})

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := rl.Middleware(ok)

	// First request succeeds
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket/key", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("first request: got %d, want 200", rec.Code)
	}

	// Second request should be rate-limited
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("second request: got %d, want 429", rec2.Code)
	}
}

func TestRateLimiter_Middleware429_IncrementsMetric(t *testing.T) {
	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1,
		Burst:          1,
	})

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := rl.Middleware(ok)

	before := testutil.ToFloat64(telemetry.RateLimitRejectionsTotal)

	// Exhaust the burst
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket/key", nil)
	req.RemoteAddr = "10.0.0.99:12345"
	handler.ServeHTTP(rec, req)

	// This request should be rate-limited
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/test-bucket/key", nil)
	req2.RemoteAddr = "10.0.0.99:12345"
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec2.Code)
	}

	after := testutil.ToFloat64(telemetry.RateLimitRejectionsTotal)
	if after <= before {
		t.Errorf("RateLimitRejectionsTotal did not increment: before=%v, after=%v", before, after)
	}
}

func TestRateLimiter_UpdateLimits_NewVisitors(t *testing.T) {
	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1,
		Burst:          1,
	})
	defer rl.Close()

	// Update to a much higher rate
	rl.UpdateLimits(1000, 1000)

	// New visitor after update should get the new rate (1000 burst)
	for i := 0; i < 100; i++ {
		if !rl.Allow("10.0.0.99") {
			t.Fatalf("request %d should be allowed with new burst=1000", i+1)
		}
	}
}

func TestRateLimiter_UpdateLimits_ExistingVisitorsKeepOldRate(t *testing.T) {
	rl := NewRateLimiter(config.RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1,
		Burst:          2,
	})
	defer rl.Close()

	// Establish existing visitor with burst=2
	if !rl.Allow("10.0.0.1") {
		t.Fatal("first request should be allowed")
	}
	if !rl.Allow("10.0.0.1") {
		t.Fatal("second request (within burst) should be allowed")
	}

	// Update limits to a higher burst — existing visitor won't benefit
	rl.UpdateLimits(1, 1000)

	// Existing visitor still has the old limiter (burst=2, exhausted)
	if rl.Allow("10.0.0.1") {
		t.Error("existing visitor should still be rate-limited by old burst")
	}

	// A brand new visitor should get the new limits
	for i := 0; i < 100; i++ {
		if !rl.Allow("10.0.0.50") {
			t.Fatalf("new visitor request %d should be allowed with new burst=1000", i+1)
		}
	}
}

func TestExtractIP_NoTrustedProxies(t *testing.T) {
	rl := &RateLimiter{}

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{"ip:port", "10.0.0.1:12345", "", "10.0.0.1"},
		{"ip only", "10.0.0.1", "", "10.0.0.1"},
		{"xff ignored without trusted proxies", "10.0.0.1:12345", "192.168.1.1", "10.0.0.1"},
		{"xff chain ignored without trusted proxies", "10.0.0.1:12345", "192.168.1.1, 10.0.0.2", "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			got := rl.extractIP(r)
			if got != tt.want {
				t.Errorf("extractIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractIP_WithTrustedProxies(t *testing.T) {
	rl := &RateLimiter{
		trustedProxies: parseCIDRs([]string{"10.0.0.0/8", "172.16.0.0/12"}),
	}

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{
			"xff with trusted peer uses rightmost untrusted",
			"10.0.0.1:12345",
			"203.0.113.50, 10.0.0.5",
			"203.0.113.50",
		},
		{
			"xff chain skips trusted hops from the right",
			"10.0.0.1:12345",
			"203.0.113.50, 172.16.0.1, 10.0.0.5",
			"203.0.113.50",
		},
		{
			"all xff entries trusted falls back to leftmost",
			"10.0.0.1:12345",
			"10.1.1.1, 172.16.0.1",
			"10.1.1.1",
		},
		{
			"single xff entry with trusted peer",
			"10.0.0.1:12345",
			"203.0.113.50",
			"203.0.113.50",
		},
		{
			"untrusted peer ignores xff even with trusted_proxies configured",
			"203.0.113.99:12345",
			"1.2.3.4",
			"203.0.113.99",
		},
		{
			"no xff with trusted peer uses remote addr",
			"10.0.0.1:12345",
			"",
			"10.0.0.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			got := rl.extractIP(r)
			if got != tt.want {
				t.Errorf("extractIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseCIDRs(t *testing.T) {
	nets := parseCIDRs([]string{"10.0.0.0/8", "invalid", "192.168.0.0/16"})
	if len(nets) != 2 {
		t.Fatalf("got %d nets, want 2 (invalid should be skipped)", len(nets))
	}
}
