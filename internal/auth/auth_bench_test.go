package auth

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// benchSignedRequest constructs a valid SigV4-signed request for benchmarking.
func benchSignedRequest(accessKey, secret string) *http.Request {
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]

	r, _ := http.NewRequest("GET", "/bucket/key", nil)
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	r.Host = "localhost"

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonicalRequest := buildCanonicalRequest(r, signedHeaders)
	credentialScope := fmt.Sprintf("%s/us-east-1/s3/aws4_request", dateStamp)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, credentialScope, hashSHA256([]byte(canonicalRequest)))
	signingKey := deriveSigningKey(secret, dateStamp, "us-east-1", "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	r.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+accessKey+"/"+credentialScope+
			", SignedHeaders=host;x-amz-content-sha256;x-amz-date"+
			", Signature="+signature)

	return r
}

func BenchmarkVerifySigV4(b *testing.B) {
	accessKey := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	r := benchSignedRequest(accessKey, secret)

	b.ResetTimer()
	for b.Loop() {
		_ = VerifySigV4(r, accessKey, secret)
	}
}

func BenchmarkDeriveSigningKey(b *testing.B) {
	for b.Loop() {
		deriveSigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20260215", "us-east-1", "s3")
	}
}

func BenchmarkParseSigV4Fields(b *testing.B) {
	input := "Credential=AKID/20260215/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	for b.Loop() {
		parseSigV4Fields(input)
	}
}
