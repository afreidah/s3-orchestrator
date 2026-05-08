// -------------------------------------------------------------------------------
// Streaming SigV4 Bypass - Rejection Regression Test
//
// Author: Alex Freidah
//
// Constructs a hand-crafted PUT whose Authorization header is signed
// with the STREAMING-AWS4-HMAC-SHA256-PAYLOAD sentinel as the
// canonical-request payload hash but whose chunk-signature values are
// zeros (the chain is fake). The orchestrator must reject the request
// with 403 SignatureDoesNotMatch before any body bytes reach storage.
// Pre-fix behaviour was acceptance plus on-disk corruption; this test
// locks in the post-fix rejection.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// streamingPayloadSentinel is the AWS SigV4 marker for chunk-signed
// payloads.
const streamingPayloadSentinel = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"

// TestStreamingSigV4_BypassRejected asserts the orchestrator now
// rejects a request whose seed signature verifies but whose per-chunk
// signatures are bogus.
func TestStreamingSigV4_BypassRejected(t *testing.T) {
	resetState(t)
	ctx := context.Background()

	const (
		bucket    = virtualBucket
		key       = "streaming-bypass-test"
		accessKey = "test"
		secretKey = "test"
		region    = "us-east-1"
		service   = "s3"
		plaintext = "hello world"
	)

	body := chunkedBody([]byte(plaintext))
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

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

	credScope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credScope + "\n" + sha256Hex([]byte(canonicalRequest))
	kSigning := deriveTestSigningKey(secretKey, dateStamp, region, service)
	seedSig := hex.EncodeToString(hmacSHA256Bytes(kSigning, []byte(stringToSign)))

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

	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	respBody, _ := io.ReadAll(putResp.Body)
	_ = putResp.Body.Close()

	if putResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got status=%d body=%s", putResp.StatusCode, respBody)
	}
	if !bytes.Contains(respBody, []byte("SignatureDoesNotMatch")) {
		t.Errorf("expected SignatureDoesNotMatch in error body, got: %s", respBody)
	}

	if _, err := tryGetObjectBytes(ctx, t, bucket, key); err == nil {
		t.Error("object should not exist; bypass write must have been rejected before any storage write")
	}
}

// chunkedBody wraps payload in a single aws-chunked frame plus the
// terminating zero-size chunk. Chunk-signature values are zero because the
// orchestrator does not validate them today.
func chunkedBody(payload []byte) []byte {
	var b bytes.Buffer
	zeroSig := strings.Repeat("0", 64)
	fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(payload), zeroSig)
	b.Write(payload)
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "0;chunk-signature=%s\r\n\r\n", zeroSig)
	return b.Bytes()
}

// hmacSHA256Bytes computes HMAC-SHA256.
func hmacSHA256Bytes(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// sha256Hex computes the hex-encoded SHA256 digest of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// deriveTestSigningKey reproduces the SigV4 signing-key derivation locally
// so the test does not depend on unexported helpers in the auth package.
func deriveTestSigningKey(secret, dateStamp, region, service string) []byte {
	k := hmacSHA256Bytes([]byte("AWS4"+secret), []byte(dateStamp))
	k = hmacSHA256Bytes(k, []byte(region))
	k = hmacSHA256Bytes(k, []byte(service))
	return hmacSHA256Bytes(k, []byte("aws4_request"))
}

// tryGetObjectBytes reads the named object via the integration suite's
// S3 client without failing the test on absence; the caller decides
// whether absence is the desired outcome.
func tryGetObjectBytes(ctx context.Context, t *testing.T, bucket, key string) ([]byte, error) {
	t.Helper()
	client := newS3Client(t)
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}
