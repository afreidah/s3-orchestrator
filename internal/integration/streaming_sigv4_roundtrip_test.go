// -------------------------------------------------------------------------------
// Streaming SigV4 - Signed Round-Trip and Tampered-Chunk Tests
//
// Author: Alex Freidah
//
// End-to-end coverage for the streaming chunk validator: a PUT carrying
// a properly chained per-chunk signature must round-trip byte-equal,
// and a single flipped byte in the chunk body must be rejected with
// 403 SignatureDoesNotMatch. Together with the bypass test these
// exercise the three observable wire outcomes (accept, malformed
// reject, signature-mismatch reject).
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// emptyStringSHA256 is hex(SHA256("")), used in the per-chunk
// string-to-sign as the hash of the empty fixed field.
const emptyStringSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// signedStreamingRequest captures the fully-formed PUT plus the body
// bytes so a test can mutate them before sending.
type signedStreamingRequest struct {
	req  *http.Request
	body []byte
}

// buildSignedStreamingPUT crafts a PUT request whose seed signature
// verifies and whose body is a valid chained-chunk-signed payload of
// the supplied plaintext. The caller may mutate body before sending
// (e.g. flip a byte to drive the tampered-chunk negative test).
func buildSignedStreamingPUT(
	ctx context.Context,
	t *testing.T,
	bucket, key, accessKey, secretKey, plaintext string,
) signedStreamingRequest {
	t.Helper()
	const (
		region  = "us-east-1"
		service = "s3"
	)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	credScope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	signingKey := deriveTestSigningKey(secretKey, dateStamp, region, service)

	body := buildValidSignedChunkBody([]byte(plaintext), 128, signingKey, "", credScope, amzDate)

	canonicalURI := "/" + bucket + "/" + key
	signedHeaders := "content-encoding;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length"
	canonicalHeaders := fmt.Sprintf(
		"content-encoding:aws-chunked\nhost:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\nx-amz-decoded-content-length:%d\n",
		proxyAddr, streamingPayloadSentinel, amzDate, len(plaintext),
	)
	canonicalRequest := strings.Join([]string{
		http.MethodPut,
		canonicalURI,
		"",
		canonicalHeaders,
		signedHeaders,
		streamingPayloadSentinel,
	}, "\n")

	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credScope + "\n" + sha256Hex([]byte(canonicalRequest))
	seedSig := hex.EncodeToString(hmacSHA256Bytes(signingKey, []byte(stringToSign)))

	body = buildValidSignedChunkBody([]byte(plaintext), 128, signingKey, seedSig, credScope, amzDate)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://"+proxyAddr+canonicalURI, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Content-Sha256", streamingPayloadSentinel)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(plaintext)))
	req.ContentLength = int64(len(body))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, credScope, signedHeaders, seedSig,
	))
	return signedStreamingRequest{req: req, body: body}
}

// buildValidSignedChunkBody emits the wire-format body for a
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD upload of payload, chained from
// seedSig. Pass an empty seedSig on the first call when only the body
// length is needed (the returned bytes are still well-formed but the
// chain root is not yet bound to the request).
func buildValidSignedChunkBody(payload []byte, chunkSize int, signingKey []byte, seedSig, credScope, amzDate string) []byte {
	var b bytes.Buffer
	prevSig := seedSig
	for off := 0; off < len(payload); off += chunkSize {
		end := min(off+chunkSize, len(payload))
		chunk := payload[off:end]
		sig := computeChunkSignatureForTest(signingKey, amzDate, credScope, prevSig, chunk)
		fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(chunk), sig)
		b.Write(chunk)
		b.WriteString("\r\n")
		prevSig = sig
	}
	finalSig := computeChunkSignatureForTest(signingKey, amzDate, credScope, prevSig, nil)
	fmt.Fprintf(&b, "0;chunk-signature=%s\r\n\r\n", finalSig)
	return b.Bytes()
}

// computeChunkSignatureForTest mirrors the per-chunk string-to-sign so
// fixtures produce signatures the orchestrator's reader will accept.
func computeChunkSignatureForTest(key []byte, amzDate, scope, prevSig string, chunk []byte) string {
	chunkHash := sha256.Sum256(chunk)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256-PAYLOAD",
		amzDate,
		scope,
		prevSig,
		emptyStringSHA256,
		hex.EncodeToString(chunkHash[:]),
	}, "\n")
	return hex.EncodeToString(hmacSHA256Bytes(key, []byte(stringToSign)))
}

// TestStreamingSigV4_SignedRoundTrip drives a properly signed streaming
// PUT through the orchestrator and asserts the GET returns the
// plaintext byte-for-byte.
func TestStreamingSigV4_SignedRoundTrip(t *testing.T) {
	resetState(t)
	ctx := context.Background()

	const (
		bucket    = virtualBucket
		key       = "streaming-roundtrip"
		accessKey = "test"
		secretKey = "test"
	)
	// Test backends in this fixture cap at 2 KiB so the payload must fit;
	// 300 bytes with a 128-byte chunk size still exercises the multi-chunk
	// chain (3 body chunks plus the zero terminator).
	plaintext := strings.Repeat("hello streaming world!\n", 13)

	rb := buildSignedStreamingPUT(ctx, t, bucket, key, accessKey, secretKey, plaintext)
	resp, err := http.DefaultClient.Do(rb.req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT failed: status=%d body=%s", resp.StatusCode, body)
	}

	got, err := tryGetObjectBytes(ctx, t, bucket, key)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if !bytes.Equal(got, []byte(plaintext)) {
		t.Errorf("body mismatch:\nwant len=%d\ngot  len=%d", len(plaintext), len(got))
	}
}

// TestStreamingSigV4_TamperedChunkRejected sends a properly signed
// streaming PUT but flips one byte in the first chunk's payload. The
// orchestrator must reject with 403 SignatureDoesNotMatch.
func TestStreamingSigV4_TamperedChunkRejected(t *testing.T) {
	resetState(t)
	ctx := context.Background()

	const (
		bucket    = virtualBucket
		key       = "streaming-tampered"
		accessKey = "test"
		secretKey = "test"
		plaintext = "tamper me"
	)
	rb := buildSignedStreamingPUT(ctx, t, bucket, key, accessKey, secretKey, plaintext)

	idx := bytes.Index(rb.body, []byte("\r\n"))
	if idx < 0 || idx+2 >= len(rb.body) {
		t.Fatalf("could not locate chunk body to tamper")
	}
	tampered := append([]byte(nil), rb.body...)
	tampered[idx+2] ^= 0x01
	rb.req.Body = io.NopCloser(bytes.NewReader(tampered))
	rb.req.ContentLength = int64(len(tampered))

	resp, err := http.DefaultClient.Do(rb.req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got status=%d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("SignatureDoesNotMatch")) {
		t.Errorf("expected SignatureDoesNotMatch, got: %s", body)
	}
	if _, err := tryGetObjectBytes(ctx, t, bucket, key); err == nil {
		t.Error("object should not exist after rejected upload")
	}
}
