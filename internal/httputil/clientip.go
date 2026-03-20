// -------------------------------------------------------------------------------
// Client IP Extraction
//
// Author: Alex Freidah
//
// Extracts the real client IP from requests, supporting X-Forwarded-For with
// trusted proxy validation. Shared by the rate limiter and UI login throttle.
// -------------------------------------------------------------------------------

package httputil

import (
	"net"
	"net/http"
	"strings"
)

// ExtractClientIP returns the client's real IP address. When the direct peer
// is a trusted proxy and X-Forwarded-For is present, the rightmost untrusted
// IP is returned. Otherwise the direct peer IP is returned.
func ExtractClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	peerIP := stripPort(r.RemoteAddr)

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" &&
		len(trustedProxies) > 0 && ipInNets(peerIP, trustedProxies) {
		return rightmostUntrusted(xff, trustedProxies)
	}

	return peerIP
}

// ParseTrustedProxies parses CIDR strings into net.IPNet slices.
// Invalid CIDRs are silently skipped (they are caught during config
// validation).
func ParseTrustedProxies(cidrs []string) []*net.IPNet {
	var nets []*net.IPNet
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err == nil {
			nets = append(nets, ipNet)
		}
	}
	return nets
}

// stripPort removes the port suffix from an address string.
func stripPort(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// ipInNets returns true if the given IP string falls within any of the
// provided networks.
func ipInNets(ipStr string, nets []*net.IPNet) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// rightmostUntrusted walks the X-Forwarded-For chain from right to left and
// returns the first IP that is not in a trusted network. If all IPs are
// trusted, returns the leftmost (original client).
func rightmostUntrusted(xff string, trusted []*net.IPNet) string {
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(parts[i])
		if ip == "" {
			continue
		}
		if !ipInNets(ip, trusted) {
			return ip
		}
	}
	// All trusted — return leftmost
	if first := strings.TrimSpace(parts[0]); first != "" {
		return first
	}
	return ""
}
