// -------------------------------------------------------------------------------
// Signed Token Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package httputil

import (
	"encoding/base64"
	"testing"
	"time"
)

var (
	tokenKey = []byte("test-key")
	tokenNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
)

// TestSignedToken_RoundTrip verifies a token verifies before its expiry and
// returns the payload it was signed with, including one containing the
// separator.
func TestSignedToken_RoundTrip(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{"purge|b1", "user|AKIA|with|bars", ""} {
		token := SignToken(tokenKey, payload, tokenNow.Add(time.Minute))
		got, ok := VerifyToken(tokenKey, token, tokenNow)
		if !ok || got != payload {
			t.Errorf("VerifyToken(%q) = (%q, %v), want (%q, true)", payload, got, ok, payload)
		}
	}
}

// TestSignedToken_ExpiresAtExpiry verifies a token stops verifying once now
// reaches its expiry.
func TestSignedToken_ExpiresAtExpiry(t *testing.T) {
	t.Parallel()
	expiry := tokenNow.Add(time.Minute)
	token := SignToken(tokenKey, "p", expiry)
	if _, ok := VerifyToken(tokenKey, token, expiry.Add(-time.Second)); !ok {
		t.Error("token rejected before its expiry")
	}
	if _, ok := VerifyToken(tokenKey, token, expiry); ok {
		t.Error("token accepted at its expiry")
	}
}

// TestSignedToken_RejectsForgery verifies a token signed under another key, or
// altered after signing, does not verify.
func TestSignedToken_RejectsForgery(t *testing.T) {
	t.Parallel()
	token := SignToken(tokenKey, "p", tokenNow.Add(time.Minute))
	if _, ok := VerifyToken([]byte("other-key"), token, tokenNow); ok {
		t.Error("token verified under the wrong key")
	}
	if _, ok := VerifyToken(tokenKey, token+"x", tokenNow); ok {
		t.Error("altered token verified")
	}
	forged := SignToken([]byte("other-key"), "p", tokenNow.Add(time.Minute))
	if _, ok := VerifyToken(tokenKey, forged, tokenNow); ok {
		t.Error("token signed with another key verified")
	}
}

// TestSignedToken_RejectsMalformed verifies the shapes that fail before the
// signature check.
func TestSignedToken_RejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"", "nodot", "!!!.sig", "cGF5bG9hZA.!!!"} {
		if _, ok := VerifyToken(tokenKey, token, tokenNow); ok {
			t.Errorf("VerifyToken(%q) accepted a malformed token", token)
		}
	}
}

// TestSignedToken_RejectsBadExpiry verifies a correctly signed value with no
// expiry field, or one that is not a number, does not verify.
func TestSignedToken_RejectsBadExpiry(t *testing.T) {
	t.Parallel()
	for _, signed := range []string{"noexpiry", "p|soon"} {
		token := base64.RawURLEncoding.EncodeToString([]byte(signed)) + "." +
			base64.RawURLEncoding.EncodeToString(tokenMAC(tokenKey, signed))
		if _, ok := VerifyToken(tokenKey, token, tokenNow); ok {
			t.Errorf("VerifyToken accepted signed value %q", signed)
		}
	}
}
