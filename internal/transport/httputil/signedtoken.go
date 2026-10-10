// -------------------------------------------------------------------------------
// Signed Tokens
//
// Author: Alex Freidah
//
// An expiring HMAC-SHA256 token for state the server hands a client and must
// trust when it comes back: the admin purge confirmation and the UI session
// cookie. The token is base64url(payload|expiry) "." base64url(mac), with the
// expiry in unix seconds. Each caller supplies its own key and payload fields.
// -------------------------------------------------------------------------------

package httputil

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// SignToken signs payload with key so it verifies until expiry.
func SignToken(key []byte, payload string, expiry time.Time) string {
	signed := payload + "|" + strconv.FormatInt(expiry.Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(signed)) + "." +
		base64.RawURLEncoding.EncodeToString(tokenMAC(key, signed))
}

// VerifyToken checks a token's signature against key and its expiry against
// now, and returns the payload it was signed with. A malformed, forged or
// expired token reports false.
func VerifyToken(key []byte, token string, now time.Time) (string, bool) {
	encoded, encodedSig, found := strings.Cut(token, ".")
	if !found {
		return "", false
	}
	signed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(encodedSig)
	if err != nil {
		return "", false
	}
	if !hmac.Equal(tokenMAC(key, string(signed)), sig) {
		return "", false
	}
	payload, rawExpiry, found := strings.CutLast(string(signed), "|")
	if !found {
		return "", false
	}
	expiry, err := strconv.ParseInt(rawExpiry, 10, 64)
	if err != nil || now.Unix() >= expiry {
		return "", false
	}
	return payload, true
}

// tokenMAC is the HMAC-SHA256 of s under key.
func tokenMAC(key []byte, s string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(s))
	return mac.Sum(nil)
}
