// -------------------------------------------------------------------------------
// Rate Limiter Fuzz Tests - Client IP Extraction
//
// Author: Alex Freidah
//
// Fuzz tests for ExtractClientIP, which parses RemoteAddr and X-Forwarded-For
// headers to determine the real client IP. Validates that malformed addresses,
// IPv6 brackets, empty chains, and adversarial XFF values never cause panics.
// -------------------------------------------------------------------------------

package s3api

import (
	"net"
	"context"
	"net/http"
	"testing"
)

func FuzzExtractClientIP(f *testing.F) {
	f.Add("192.168.1.1:8080", "10.0.0.1, 172.16.0.1, 192.168.1.1")
	f.Add("[::1]:443", "2001:db8::1, ::ffff:10.0.0.1")
	f.Add("10.0.0.1:1234", "")
	f.Add("not-an-address", "garbage, more garbage")
	f.Add("", "")
	f.Add(":", "10.0.0.1")
	f.Add("10.0.0.1:", ",,,")
	f.Add("[::1]:0", "  , , 10.0.0.1 , ")
	f.Add("10.0.0.1:80", "10.0.0.1, [::1]")
	f.Add("192.168.1.1:443", "    ")
	f.Add("127.0.0.1:9000", "spoofed, 10.0.0.1, 192.168.1.100")

	_, trusted, _ := net.ParseCIDR("192.168.0.0/16")
	trustedNets := []*net.IPNet{trusted}

	f.Fuzz(func(t *testing.T, remoteAddr, xff string) {
		r, err := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
		if err != nil {
			return
		}
		r.RemoteAddr = remoteAddr
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}

		// Must not panic with no trusted proxies.
		ExtractClientIP(r, nil)

		// Must not panic with trusted proxies.
		ExtractClientIP(r, trustedNets)
	})
}
