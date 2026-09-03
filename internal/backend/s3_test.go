// -------------------------------------------------------------------------------
// Backend Tests - S3 Client Configuration
//
// Author: Alex Freidah
//
// Unit tests for S3 backend client construction and option helpers.
// -------------------------------------------------------------------------------

package backend

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// nonSeekableReader wraps a reader so it cannot be type-asserted to
// io.ReadSeeker, forcing PutObject's signed-payload materialization path.
type nonSeekableReader struct{ r io.Reader }

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func (n nonSeekableReader) Read(p []byte) (int, error) { return n.r.Read(p) }

// TestPreparePutBody_UnsignedStreamsDirectly covers the unsigned-payload path:
// the body is passed through untouched and tagged with the unsigned option.
func TestPreparePutBody_UnsignedStreamsDirectly(t *testing.T) {
	t.Parallel()
	payload := []byte("prepare-put-body-payload")
	b := &S3Backend{unsignedPayload: true}

	got, opts, cleanup, err := b.preparePutBody(nonSeekableReader{r: bytes.NewReader(payload)}, int64(len(payload)))
	if err != nil {
		t.Fatalf("preparePutBody: %v", err)
	}
	defer cleanup()

	if len(opts) != 1 {
		t.Errorf("opts = %d, want 1 (withUnsignedPayload)", len(opts))
	}
	if data, _ := io.ReadAll(got); !bytes.Equal(data, payload) {
		t.Errorf("body = %q, want the original payload passed through", data)
	}
}

// TestPutObject_PipeBodyStillSendsContentLength is the regression guard for the
// multipart assembly failure: the assembly body is an *io.PipeReader, and
// smithy-go's request builder overwrites ContentLength with -1 for that one
// concrete type. The upload then goes out chunked with no Content-Length while
// SigV4 has already signed the header, which no S3 implementation accepts.
//
// The assertion is deliberately on the wire rather than on preparePutBody's
// return value. What preparePutBody returns is not the property that matters;
// what reaches the backend is, and that is what a future SDK bump can silently
// change.
func TestPutObject_PipeBodyStillSendsContentLength(t *testing.T) {
	t.Parallel()

	var (
		gotLength   int64
		gotEncoding []string
		gotBody     []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLength, gotEncoding = r.ContentLength, r.TransferEncoding
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	unsigned := true
	be, err := NewS3Backend(t.Context(), &config.BackendConfig{
		Name:            "test",
		Endpoint:        srv.URL,
		Region:          "us-east-1",
		Bucket:          "test-bucket",
		AccessKeyID:     "AKID",
		SecretAccessKey: "secret",
		ForcePathStyle:  true,
		UnsignedPayload: &unsigned,
	})
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}

	payload := bytes.Repeat([]byte("s3o-pipe-body-"), 128)
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(payload)
		_ = pw.Close()
	}()

	if _, err := be.PutObject(t.Context(), "dir/obj", pr, int64(len(payload)), "text/plain", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if gotLength != int64(len(payload)) {
		t.Errorf("Content-Length = %d, want %d; a backend requiring it answers 411 and one checking the signature answers 403",
			gotLength, len(payload))
	}
	if len(gotEncoding) != 0 {
		t.Errorf("Transfer-Encoding = %v, want none", gotEncoding)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Errorf("backend received %d bytes, want %d", len(gotBody), len(payload))
	}
}

// TestWithKnownLength_KeepsSeekableBodiesSeekable pins the other half of the
// wrapper's contract. The single-object write path materializes precisely so
// the SDK can rewind and retry on failover; wrapping a seekable body would
// hide io.Seeker and take that away.
func TestWithKnownLength_KeepsSeekableBodiesSeekable(t *testing.T) {
	t.Parallel()

	seekable := bytes.NewReader([]byte("seekable"))
	if got := withKnownLength(seekable); got != io.Reader(seekable) {
		t.Errorf("withKnownLength wrapped a seekable body (%T); the SDK loses retry", got)
	}

	nonSeekable := nonSeekableReader{r: bytes.NewReader([]byte("stream"))}
	if _, ok := withKnownLength(nonSeekable).(io.ReadSeeker); ok {
		t.Error("withKnownLength must not invent seekability")
	}
}

// TestPreparePutBody_SignedSeekablePassthrough covers signed-payload mode with
// an already-seekable body: it is passed through unchanged, not re-materialized.
func TestPreparePutBody_SignedSeekablePassthrough(t *testing.T) {
	t.Parallel()
	payload := []byte("prepare-put-body-payload")
	b := &S3Backend{unsignedPayload: false}
	seekable := bytes.NewReader(payload)

	got, opts, cleanup, err := b.preparePutBody(seekable, int64(len(payload)))
	if err != nil {
		t.Fatalf("preparePutBody: %v", err)
	}
	defer cleanup()

	if opts != nil {
		t.Errorf("signed mode should add no API options, got %d", len(opts))
	}
	if got != io.Reader(seekable) {
		t.Error("a seekable body should be passed through unchanged, not re-materialized")
	}
}

// TestPreparePutBody_SignedNonSeekableMaterializes covers signed-payload mode
// with a non-seekable body: it is materialized to a seekable form that replays
// the original bytes.
func TestPreparePutBody_SignedNonSeekableMaterializes(t *testing.T) {
	t.Parallel()
	payload := []byte("prepare-put-body-payload")
	b := &S3Backend{unsignedPayload: false}

	got, opts, cleanup, err := b.preparePutBody(nonSeekableReader{r: bytes.NewReader(payload)}, int64(len(payload)))
	if err != nil {
		t.Fatalf("preparePutBody: %v", err)
	}
	defer cleanup()

	if opts != nil {
		t.Errorf("signed mode should add no API options, got %d", len(opts))
	}
	rs, ok := got.(io.ReadSeeker)
	if !ok {
		t.Fatalf("materialized body is %T, want a seekable io.ReadSeeker", got)
	}
	if data, _ := io.ReadAll(rs); !bytes.Equal(data, payload) {
		t.Errorf("materialized body = %q, want %q", data, payload)
	}
}

// TestPutObject_SignedPayloadNonSeekableBodyMaterializes pins #972: with
// unsigned_payload disabled and a non-seekable body, PutObject materializes the
// stream to a seekable form instead of io.ReadAll-ing the whole object into
// memory, and the backend still receives every byte.
func TestPutObject_SignedPayloadNonSeekableBodyMaterializes(t *testing.T) {
	t.Parallel()

	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	be, err := NewS3Backend(t.Context(), &config.BackendConfig{
		Name:            "test",
		Endpoint:        srv.URL, // http:// forces signed payload
		Region:          "us-east-1",
		Bucket:          "test-bucket",
		AccessKeyID:     "AKID",
		SecretAccessKey: "secret",
		ForcePathStyle:  true, // reach the httptest server at /bucket/key
		DisableChecksum: true, // keep the wire body raw (no aws-chunked framing)
	})
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}
	if be.unsignedPayload {
		t.Fatal("expected signed-payload mode for the http endpoint")
	}

	payload := bytes.Repeat([]byte("s3o-signed-payload-"), 4096) // ~76 KiB
	body := nonSeekableReader{r: bytes.NewReader(payload)}

	etag, err := be.PutObject(t.Context(), "dir/obj", body, int64(len(payload)), "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if etag == "" {
		t.Error("expected an ETag back")
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("backend received %d bytes, want %d (body dropped or truncated)", len(got), len(payload))
	}
}

// TestWithUnsignedPayload_AddsAPIOption verifies the with unsigned payload adds apioption contract.
// Asserts that expected 1 API option, got.
func TestWithUnsignedPayload_AddsAPIOption(t *testing.T) {
	t.Parallel()
	var opts s3.Options
	withUnsignedPayload(&opts)

	if len(opts.APIOptions) != 1 {
		t.Fatalf("expected 1 API option, got %d", len(opts.APIOptions))
	}
}

// TestNewS3Backend_UnsignedPayloadDefaults verifies the new s3 backend unsigned payload defaults contract.
// Asserts that unexpected error:.
func TestNewS3Backend_UnsignedPayloadDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		endpoint        string
		unsignedPayload *bool
		wantUnsigned    bool
	}{
		{
			name:         "https defaults to unsigned",
			endpoint:     "https://example.com",
			wantUnsigned: true,
		},
		{
			name:         "http forces signed",
			endpoint:     "http://example.com",
			wantUnsigned: false,
		},
		{
			name:            "explicit false overrides https",
			endpoint:        "https://example.com",
			unsignedPayload: func() *bool { b := false; return &b }(),
			wantUnsigned:    false,
		},
		{
			name:            "explicit true with http is respected",
			endpoint:        "http://example.com",
			unsignedPayload: func() *bool { b := true; return &b }(),
			wantUnsigned:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, err := NewS3Backend(t.Context(), &config.BackendConfig{
				Name:            "test",
				Endpoint:        tt.endpoint,
				Region:          "us-east-1",
				Bucket:          "test-bucket",
				AccessKeyID:     "AKID",
				SecretAccessKey: "secret",
				UnsignedPayload: tt.unsignedPayload,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if backend.unsignedPayload != tt.wantUnsigned {
				t.Errorf("unsignedPayload = %v, want %v", backend.unsignedPayload, tt.wantUnsigned)
			}
		})
	}
}

// TestNewS3Backend_DisableChecksum verifies the new s3 backend disable checksum contract.
// Asserts that unexpected error:.
func TestNewS3Backend_DisableChecksum(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		disableChecksum bool
	}{
		{
			name:            "checksum disabled",
			disableChecksum: true,
		},
		{
			name:            "checksum enabled (default)",
			disableChecksum: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewS3Backend(t.Context(), &config.BackendConfig{
				Name:            "test",
				Endpoint:        "https://storage.googleapis.com",
				Region:          "us",
				Bucket:          "test-bucket",
				AccessKeyID:     "AKID",
				SecretAccessKey: "secret",
				DisableChecksum: tt.disableChecksum,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestNewS3Backend_DefaultChainConstructs pins that a backend with
// CredentialSource=default_chain wires the AWS SDK default-chain
// provider instead of static keys. Construction must succeed; actual
// resolution happens lazily on first request and is exercised by the
// integration suite when run against an AWS-attached instance.
func TestNewS3Backend_DefaultChainConstructs(t *testing.T) {
	t.Parallel()
	be, err := NewS3Backend(t.Context(), &config.BackendConfig{
		Name:             "test-default-chain",
		Endpoint:         "https://s3.amazonaws.com",
		Region:           "us-east-1",
		Bucket:           "test-bucket",
		CredentialSource: config.CredentialSourceDefaultChain,
	})
	if err != nil {
		t.Fatalf("default_chain construction failed: %v", err)
	}
	if be == nil {
		t.Fatal("default_chain returned nil backend")
	}
}

// TestNewS3Backend_StripSDKHeaders verifies the new s3 backend strip sdkheaders contract.
// Asserts that unexpected error:.
func TestNewS3Backend_StripSDKHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		stripSDKHeaders bool
	}{
		{
			name:            "strip enabled",
			stripSDKHeaders: true,
		},
		{
			name:            "strip disabled (default)",
			stripSDKHeaders: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewS3Backend(t.Context(), &config.BackendConfig{
				Name:            "test",
				Endpoint:        "https://storage.googleapis.com",
				Region:          "auto",
				Bucket:          "test-bucket",
				AccessKeyID:     "AKID",
				SecretAccessKey: "secret",
				StripSDKHeaders: tt.stripSDKHeaders,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
