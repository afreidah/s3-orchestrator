// -------------------------------------------------------------------------------
// Integration Tests - Backends That Ignore Range
//
// Author: Alex Freidah
//
// Some S3 gateways answer a ranged GET with 200 and the whole object. A proxy
// in front of MinIO plays that backend here, and every stored form is read
// whole and by range through it, byte for byte against what was written.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// noRangeBackend names the one backend in each fleet below, so every read is
// served by the backend that ignores Range.
const noRangeBackend = "h-no-range"

// rangeIgnoringProxy fronts a MinIO endpoint with a proxy that drops the Range
// header from every request, so MinIO answers each ranged GET with the whole
// object. SigV4 signs Range, so a request that loses it is signed again with
// the harness credentials. Returns the proxy's endpoint.
func rangeIgnoringProxy(t *testing.T, endpoint string) string {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse MinIO endpoint %s: %v", endpoint, err)
	}
	// S3 signs the path as sent, without the second escaping SigV4 applies to
	// other services; the stored keys carry "!" and would not match otherwise.
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: "minioadmin", SecretAccessKey: "minioadmin"}

	proxy := httputil.NewSingleHostReverseProxy(target)
	direct := proxy.Director
	proxy.Director = func(r *http.Request) {
		direct(r)
		if r.Header.Get("Range") == "" {
			return
		}
		r.Header.Del("Range")
		r.Header.Del("Authorization")
		payloadHash := r.Header.Get("X-Amz-Content-Sha256")
		if err := signer.SignHTTP(r.Context(), creds, r, payloadHash, "s3", "us-east-1", time.Now()); err != nil {
			t.Errorf("re-sign request without Range: %v", err)
		}
	}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestIgnoredRange_EveryStoredFormReadsCorrectly reads plaintext, encrypted,
// and compressed-and-encrypted objects whole and by range from a backend that
// ignores Range, and requires the backend layer to have cut every range.
func TestIgnoredRange_EveryStoredFormReadsCorrectly(t *testing.T) {
	cases := []struct {
		name string
		spec harnessSpec
	}{
		{name: "plaintext"},
		{name: "encrypted", spec: harnessSpec{Encrypt: true}},
		{name: "compressed and encrypted", spec: harnessSpec{Compression: compressionOn(), Encrypt: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			spec.Backends = []harnessBackend{{Name: noRangeBackend, Quota: 64 << 20, IgnoreRange: true}}
			h := newHarness(t, spec)

			key := uniqueKey(t, "no-range")
			body := partlyCompressible(compTestChunk * 4)
			h.put(key, body)

			if got := h.get(key); !bytes.Equal(got, body) {
				t.Fatalf("whole-object read returned %d bytes, want %d", len(got), len(body))
			}
			ignored := telemetry.BackendRangeIgnoredTotal.WithLabelValues(noRangeBackend)
			before := testutil.ToFloat64(ignored)
			assertRanges(t, h, key, body)
			if after := testutil.ToFloat64(ignored); after <= before {
				t.Errorf("no ranged read reached the backend ignoring Range: counter %v -> %v", before, after)
			}
		})
	}
}
