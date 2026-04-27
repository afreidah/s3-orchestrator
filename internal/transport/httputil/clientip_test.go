// -------------------------------------------------------------------------------
// Client IP Extraction Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package httputil

import (
	"context"
	"crypto/tls"
	"net/http"
	"testing"
)

// TestIsTLSRequest_DirectTLS covers the easy case: r.TLS is non-nil because
// the listener terminated the TLS handshake itself.
func TestIsTLSRequest_DirectTLS(t *testing.T) {
	t.Parallel()
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	r.TLS = &tls.ConnectionState{} // signals TLS-terminated peer
	if !IsTLSRequest(r, nil) {
		t.Error("IsTLSRequest with r.TLS set should return true")
	}
}

// TestIsTLSRequest_TrustedProxyHTTPS covers the proxy-terminated TLS case:
// the orchestrator sees plain HTTP from a trusted proxy that forwarded
// X-Forwarded-Proto: https.
func TestIsTLSRequest_TrustedProxyHTTPS(t *testing.T) {
	t.Parallel()
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:443"
	r.Header.Set("X-Forwarded-Proto", "https")
	if !IsTLSRequest(r, trusted) {
		t.Error("trusted proxy + XFP=https should report TLS")
	}
}

// TestIsTLSRequest_UntrustedProxyHTTPS guards against header spoofing: an
// untrusted peer claiming X-Forwarded-Proto: https must NOT be honoured.
func TestIsTLSRequest_UntrustedProxyHTTPS(t *testing.T) {
	t.Parallel()
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:443" // outside trusted range
	r.Header.Set("X-Forwarded-Proto", "https")
	if IsTLSRequest(r, trusted) {
		t.Error("untrusted peer must not be able to spoof TLS via XFP")
	}
}

// TestIsTLSRequest_PlainHTTP confirms the default plain-HTTP case returns
// false and does not panic when no trusted proxies are configured.
func TestIsTLSRequest_PlainHTTP(t *testing.T) {
	t.Parallel()
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:80"
	if IsTLSRequest(r, nil) {
		t.Error("plain HTTP with no proxies should return false")
	}
}

// TestIsTLSRequest_TrustedProxyXFPMissing covers the trusted-proxy case
// without an X-Forwarded-Proto header. Without the header we cannot tell
// whether the original hop was TLS, so the result is false.
func TestIsTLSRequest_TrustedProxyXFPMissing(t *testing.T) {
	t.Parallel()
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:80"
	if IsTLSRequest(r, trusted) {
		t.Error("missing XFP header should not infer TLS")
	}
}

func TestExtractClientIP_NoProxy(t *testing.T) {
	t.Parallel()
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:5678"

	ip := ExtractClientIP(r, nil)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4", ip)
	}
}

func TestExtractClientIP_UntrustedProxy(t *testing.T) {
	t.Parallel()
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:5678" // not in trusted range
	r.Header.Set("X-Forwarded-For", "9.9.9.9")

	ip := ExtractClientIP(r, trusted)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4 (XFF ignored for untrusted peer)", ip)
	}
}

func TestExtractClientIP_TrustedProxy(t *testing.T) {
	t.Parallel()
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5678"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.2")

	ip := ExtractClientIP(r, trusted)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4 (rightmost untrusted)", ip)
	}
}

func TestExtractClientIP_AllTrusted(t *testing.T) {
	t.Parallel()
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8", "172.16.0.0/12"})
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5678"
	r.Header.Set("X-Forwarded-For", "10.0.0.5, 172.16.0.1")

	ip := ExtractClientIP(r, trusted)
	if ip != "10.0.0.5" {
		t.Errorf("got %q, want 10.0.0.5 (leftmost fallback)", ip)
	}
}

func TestExtractClientIP_NoPort(t *testing.T) {
	t.Parallel()
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	r.RemoteAddr = "1.2.3.4"

	ip := ExtractClientIP(r, nil)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4", ip)
	}
}

func TestParseTrustedProxies_Valid(t *testing.T) {
	t.Parallel()
	nets := ParseTrustedProxies([]string{"10.0.0.0/8", "172.16.0.0/12"})
	if len(nets) != 2 {
		t.Fatalf("expected 2 nets, got %d", len(nets))
	}
}

func TestParseTrustedProxies_Invalid(t *testing.T) {
	t.Parallel()
	nets := ParseTrustedProxies([]string{"10.0.0.0/8", "invalid", "192.168.0.0/16"})
	if len(nets) != 2 {
		t.Fatalf("expected 2 nets (invalid skipped), got %d", len(nets))
	}
}

func TestParseTrustedProxies_Empty(t *testing.T) {
	t.Parallel()
	nets := ParseTrustedProxies(nil)
	if len(nets) != 0 {
		t.Fatalf("expected 0 nets, got %d", len(nets))
	}
}

func TestRightmostUntrusted_SingleIP(t *testing.T) {
	t.Parallel()
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	ip := rightmostUntrusted("1.2.3.4", trusted)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4", ip)
	}
}

func TestRightmostUntrusted_EmptyEntries(t *testing.T) {
	t.Parallel()
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	ip := rightmostUntrusted("1.2.3.4, , 10.0.0.1", trusted)
	if ip != "1.2.3.4" {
		t.Errorf("got %q, want 1.2.3.4", ip)
	}
}

func TestIpInNets_InvalidIP(t *testing.T) {
	t.Parallel()
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	if ipInNets("not-an-ip", trusted) {
		t.Error("invalid IP should not match any net")
	}
}

func TestStripPort_IPv6(t *testing.T) {
	t.Parallel()
	ip := stripPort("[::1]:8080")
	if ip != "::1" {
		t.Errorf("got %q, want ::1", ip)
	}
}
