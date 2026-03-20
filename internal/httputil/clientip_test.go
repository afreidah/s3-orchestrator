// -------------------------------------------------------------------------------
// Client IP Extraction Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package httputil

import (
	"net/http"
	"testing"
)

func TestExtractClientIP_NoProxy(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:5678"

	ip := ExtractClientIP(r, nil)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4", ip)
	}
}

func TestExtractClientIP_UntrustedProxy(t *testing.T) {
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:5678" // not in trusted range
	r.Header.Set("X-Forwarded-For", "9.9.9.9")

	ip := ExtractClientIP(r, trusted)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4 (XFF ignored for untrusted peer)", ip)
	}
}

func TestExtractClientIP_TrustedProxy(t *testing.T) {
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5678"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.2")

	ip := ExtractClientIP(r, trusted)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4 (rightmost untrusted)", ip)
	}
}

func TestExtractClientIP_AllTrusted(t *testing.T) {
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8", "172.16.0.0/12"})
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5678"
	r.Header.Set("X-Forwarded-For", "10.0.0.5, 172.16.0.1")

	ip := ExtractClientIP(r, trusted)
	if ip != "10.0.0.5" {
		t.Errorf("got %q, want 10.0.0.5 (leftmost fallback)", ip)
	}
}

func TestExtractClientIP_NoPort(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.2.3.4"

	ip := ExtractClientIP(r, nil)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4", ip)
	}
}

func TestParseTrustedProxies_Valid(t *testing.T) {
	nets := ParseTrustedProxies([]string{"10.0.0.0/8", "172.16.0.0/12"})
	if len(nets) != 2 {
		t.Fatalf("expected 2 nets, got %d", len(nets))
	}
}

func TestParseTrustedProxies_Invalid(t *testing.T) {
	nets := ParseTrustedProxies([]string{"10.0.0.0/8", "invalid", "192.168.0.0/16"})
	if len(nets) != 2 {
		t.Fatalf("expected 2 nets (invalid skipped), got %d", len(nets))
	}
}

func TestParseTrustedProxies_Empty(t *testing.T) {
	nets := ParseTrustedProxies(nil)
	if len(nets) != 0 {
		t.Fatalf("expected 0 nets, got %d", len(nets))
	}
}

func TestRightmostUntrusted_SingleIP(t *testing.T) {
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	ip := rightmostUntrusted("1.2.3.4", trusted)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4", ip)
	}
}

func TestRightmostUntrusted_EmptyEntries(t *testing.T) {
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	ip := rightmostUntrusted("1.2.3.4, , 10.0.0.1", trusted)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4", ip)
	}
}

func TestIpInNets_InvalidIP(t *testing.T) {
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	if ipInNets("not-an-ip", trusted) {
		t.Error("invalid IP should not match any net")
	}
}

func TestStripPort_IPv6(t *testing.T) {
	ip := stripPort("[::1]:8080")
	if ip != "::1" {
		t.Errorf("got %q, want ::1", ip)
	}
}
