// -------------------------------------------------------------------------------
// Encryption Helper Tests
//
// Author: Alex Freidah
//
// Verifies the proxy-side adapters that bridge the object manager to the
// chunked encryption package: the on-write wrap of the upload reader,
// the on-read unwrap and ciphertext range plumbing, and the metadata
// projection that records encrypted, encryption_key, key_id, and
// plaintext_size into object_locations. These adapters are the seam
// where plaintext sizes and ciphertext ranges have to stay perfectly
// aligned, so the tests are exhaustive across encrypted/unencrypted
// branches.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// countingKeyProvider wraps a real KeyProvider and counts WrapDEK calls so
// tests can assert that the DEK cache short-circuits the KeyProvider on
// retries. wrapErr, when set, returns an error from WrapDEK without
// incrementing the counter, simulating a Vault failure.
type countingKeyProvider struct {
	inner   encryption.KeyProvider
	wraps   atomic.Int32
	wrapErr error
}

// WrapDEK delegates to the wrapped provider but optionally returns
// wrapErr without incrementing the call counter so the test can
// simulate a Vault transit failure without losing visibility into
// the cache hit/miss accounting.
func (c *countingKeyProvider) WrapDEK(ctx context.Context, dek []byte) ([]byte, string, error) {
	if c.wrapErr != nil {
		return nil, "", c.wrapErr
	}
	c.wraps.Add(1)
	return c.inner.WrapDEK(ctx, dek)
}

// UnwrapDEK delegates to the wrapped provider; the test does not
// inject failures here because the cache covers the wrap path only.
func (c *countingKeyProvider) UnwrapDEK(ctx context.Context, wrapped []byte, keyID string) ([]byte, error) {
	return c.inner.UnwrapDEK(ctx, wrapped, keyID)
}

// KeyID returns the inner provider's KeyID so the cache key the
// encryptor builds remains stable across test calls.
func (c *countingKeyProvider) KeyID() string { return c.inner.KeyID() }

// newCountingEncryptor returns an Encryptor backed by a counting key
// provider so callers can inspect WrapDEK invocation counts.
func newCountingEncryptor(t *testing.T) (*encryption.Encryptor, *countingKeyProvider) {
	t.Helper()
	inner, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatal(err)
	}
	cp := &countingKeyProvider{inner: inner}
	enc, err := encryption.NewEncryptor(cp, 64)
	if err != nil {
		t.Fatal(err)
	}
	return enc, cp
}

// newTestEncryptor constructs a new test encryptor.
func newTestEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	p, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewEncryptor(p, 64)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// TestDecryptResponse_FullRead verifies the decrypt response full read contract.
// Asserts that encrypt:.
func TestDecryptResponse_FullRead(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	plain := []byte("hello world decryption test data")

	// Encrypt first
	encResult, err := enc.Encrypt(context.Background(), bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(encResult.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	loc := &core.ObjectLocation{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
		KeyID:         encResult.KeyID,
		PlaintextSize: int64(len(plain)),
	}

	r := &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(ciphertext)),
		Size: int64(len(ciphertext)),
	}

	err = decryptResponse(context.Background(), enc, r, loc, nil, 0, 0)
	if err != nil {
		t.Fatalf("decryptResponse: %v", err)
	}

	decrypted, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read decrypted: %v", err)
	}
	if !bytes.Equal(decrypted, plain) {
		t.Errorf("decrypted content mismatch: got %q, want %q", decrypted, plain)
	}
	if r.Size != int64(len(plain)) {
		t.Errorf("Size = %d, want %d", r.Size, len(plain))
	}
}

// TestDecryptResponse_RangeRead verifies the decrypt response range read contract.
// Asserts that encrypt:.
func TestDecryptResponse_RangeRead(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	// Build a plaintext larger than one chunk (chunk size = 64 bytes)
	plain := bytes.Repeat([]byte("A"), 256)

	// Encrypt the full object
	encResult, err := enc.Encrypt(context.Background(), bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(encResult.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	loc := &core.ObjectLocation{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
		KeyID:         encResult.KeyID,
		PlaintextSize: int64(len(plain)),
	}

	// Request a range within the plaintext
	ptStart, ptEnd := int64(10), int64(50)
	rng, err := encryption.CiphertextRange(ptStart, ptEnd, enc.ChunkSize())
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}

	// Parse the backend range to extract the ciphertext slice
	var ctStart, ctEnd int64
	if _, err := fmt.Sscanf(rng.BackendRange, "bytes=%d-%d", &ctStart, &ctEnd); err != nil {
		t.Fatalf("parse BackendRange %q: %v", rng.BackendRange, err)
	}
	ctEnd++ // BackendRange end is inclusive, slice end is exclusive
	if ctEnd > int64(len(ciphertext)) {
		ctEnd = int64(len(ciphertext))
	}
	rangeData := ciphertext[ctStart:ctEnd]

	r := &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(rangeData)),
		Size: int64(len(rangeData)),
	}

	err = decryptResponse(context.Background(), enc, r, loc, rng, ptStart, ptEnd)
	if err != nil {
		t.Fatalf("decryptResponse range: %v", err)
	}

	decrypted, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read decrypted range: %v", err)
	}

	expected := plain[ptStart : ptEnd+1]
	if !bytes.Equal(decrypted, expected) {
		t.Errorf("range mismatch: got %d bytes, want %d", len(decrypted), len(expected))
	}
	if r.ContentRange == "" {
		t.Error("expected ContentRange to be set for range request")
	}
}

// TestDecryptResponse_RangeDecryptError verifies the decrypt response range decrypt error contract.
// Asserts that encrypt:.
func TestDecryptResponse_RangeDecryptError(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)

	// Valid key data but garbage ciphertext for range decrypt
	encResult, err := enc.Encrypt(context.Background(), bytes.NewReader(bytes.Repeat([]byte("B"), 256)), 256)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	_, _ = io.ReadAll(encResult.Body)

	loc := &core.ObjectLocation{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
		KeyID:         encResult.KeyID,
		PlaintextSize: 256,
	}

	rng, _ := encryption.CiphertextRange(10, 50, enc.ChunkSize())

	r := &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader([]byte("garbage-ciphertext-for-range"))),
		Size: 28,
	}

	err = decryptResponse(context.Background(), enc, r, loc, rng, 10, 50)
	if err == nil {
		t.Fatal("expected error for corrupted range ciphertext")
	}
}

// TestDecryptResponse_DecryptError verifies the decrypt response decrypt error contract.
// Asserts that encrypt:.
func TestDecryptResponse_DecryptError(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)

	// Valid key data but garbage ciphertext  -  decrypt should fail
	encResult, err := enc.Encrypt(context.Background(), bytes.NewReader([]byte("x")), 1)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Read and discard the real ciphertext
	_, _ = io.ReadAll(encResult.Body)

	loc := &core.ObjectLocation{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
		KeyID:         encResult.KeyID,
		PlaintextSize: 1,
	}

	// Feed garbage instead of real ciphertext
	r := &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader([]byte("not-real-ciphertext-data-at-all"))),
		Size: 31,
	}

	err = decryptResponse(context.Background(), enc, r, loc, nil, 0, 0)
	if err == nil {
		t.Fatal("expected error for corrupted ciphertext")
	}
}

// -------------------------------------------------------------------------
// materializeEncrypted
// -------------------------------------------------------------------------

// TestMaterializeEncrypted_PopulatesFormAndSize verifies the envelope the
// encrypt pass reports describes the ciphertext it materialized, and that the
// DEK it wrapped cost one KeyProvider round-trip.
func TestMaterializeEncrypted_PopulatesFormAndSize(t *testing.T) {
	t.Parallel()
	enc, cp := newCountingEncryptor(t)
	plain := []byte("first call payload")

	body, ctSize, form, err := materializeEncrypted(context.Background(), enc, bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("materializeEncrypted: %v", err)
	}
	defer body.Cleanup()

	if !form.Encrypted {
		t.Error("form.Encrypted = false, want true")
	}
	if form.PlaintextSize != int64(len(plain)) {
		t.Errorf("form.PlaintextSize = %d, want %d", form.PlaintextSize, len(plain))
	}
	if form.KeyID == "" {
		t.Error("form.KeyID is empty")
	}
	if form.ContentHash != "" {
		t.Errorf("form.ContentHash = %q, want empty (caller layers integrity)", form.ContentHash)
	}
	if ctSize <= int64(len(plain)) {
		t.Errorf("ciphertext size %d should exceed plaintext %d", ctSize, len(plain))
	}
	if got := body.Size(); got != ctSize {
		t.Errorf("materialized %d bytes, form reports %d", got, ctSize)
	}
	if got := cp.wraps.Load(); got != 1 {
		t.Errorf("WrapDEK called %d times, want 1", got)
	}
}

// TestMaterializeEncrypted_EveryReaderYieldsIdenticalBytes is the invariant the
// encrypt-once pass exists for: however many copies a write places, each reads
// the one ciphertext and sends the same bytes under the same base nonce. A
// per-copy encrypt would differ on every byte and nothing downstream would
// notice, since replication copies bytes verbatim and describes the target from
// the source row.
func TestMaterializeEncrypted_EveryReaderYieldsIdenticalBytes(t *testing.T) {
	t.Parallel()
	enc, cp := newCountingEncryptor(t)
	plain := []byte("payload placed on more backends than two")

	body, _, _, err := materializeEncrypted(context.Background(), enc, bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("materializeEncrypted: %v", err)
	}
	defer body.Cleanup()

	// Four, so nothing here holds for a reason peculiar to a pair.
	const copies = 4
	var first []byte
	for i := range copies {
		r, err := body.Reader()
		if err != nil {
			t.Fatalf("copy %d reader: %v", i, err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("copy %d read: %v", i, err)
		}
		if i == 0 {
			first = got
			continue
		}
		if !bytes.Equal(got, first) {
			t.Errorf("copy %d differs from copy 0", i)
		}
	}
	if got := cp.wraps.Load(); got != 1 {
		t.Errorf("WrapDEK called %d times across %d copies, want 1", got, copies)
	}
}

// TestMaterializeEncrypted_RoundTripDecrypts verifies the materialized
// ciphertext decrypts back to the plaintext under the form's own key data.
func TestMaterializeEncrypted_RoundTripDecrypts(t *testing.T) {
	t.Parallel()
	enc, _ := newCountingEncryptor(t)
	plain := []byte("round-trip payload via materializeEncrypted")

	body, _, form, err := materializeEncrypted(context.Background(), enc, bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("materializeEncrypted: %v", err)
	}
	defer body.Cleanup()

	r, err := body.Reader()
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	_, wrappedDEK, err := encryption.UnpackKeyData(form.EncryptionKey)
	if err != nil {
		t.Fatalf("unpack key data: %v", err)
	}
	plainReader, err := enc.Decrypt(context.Background(), r, wrappedDEK, form.KeyID)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	got, err := io.ReadAll(plainReader)
	if err != nil {
		t.Fatalf("read plaintext: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, plain)
	}
}

// TestMaterializeEncrypted_WrapErrorPropagates verifies a KeyProvider failure
// surfaces with the provider error still in the chain, so the write reports why
// it could not encrypt rather than a bare "encrypt failed".
func TestMaterializeEncrypted_WrapErrorPropagates(t *testing.T) {
	t.Parallel()
	enc, cp := newCountingEncryptor(t)
	cp.wrapErr = errors.New("simulated KeyProvider failure")

	plain := []byte("doomed payload")
	body, _, _, err := materializeEncrypted(context.Background(), enc, bytes.NewReader(plain), int64(len(plain)))
	if err == nil {
		body.Cleanup()
		t.Fatal("expected error from failing KeyProvider")
	}
	if !errors.Is(err, cp.wrapErr) {
		// fmt.Errorf("encrypt: %w", err) wraps the underlying provider error.
		t.Errorf("error chain does not contain provider error: %v", err)
	}
}

// TestDecryptResponse_BadKeyData verifies the decrypt response bad key data path by exercising io.NopCloser, bytes.NewReader, context.Background.
func TestDecryptResponse_BadKeyData(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)

	loc := &core.ObjectLocation{
		Encrypted:     true,
		EncryptionKey: []byte("garbage"),
		KeyID:         "test-0",
	}
	r := &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader([]byte("ciphertext"))),
		Size: 10,
	}

	err := decryptResponse(context.Background(), enc, r, loc, nil, 0, 0)
	if err == nil {
		t.Fatal("expected error for bad key data")
	}
}

// TestStreamMetricReader_IncrementsOnNonEOFError asserts the wrapper
// fires the encryption errors counter when the underlying reader
// returns a non-EOF error mid-stream.
func TestStreamMetricReader_IncrementsOnNonEOFError(t *testing.T) {
	const op = "encrypt"
	before := testutilPromCounter(t, telemetry.EncryptionErrorsTotal.WithLabelValues(op, "stream_failed"))

	failing := iotest.ErrReader(errors.New("synthetic transport failure"))
	wrapped := withStreamMetric(failing, op)

	buf := make([]byte, 16)
	if _, err := wrapped.Read(buf); err == nil {
		t.Fatal("expected error from wrapped reader")
	}

	after := testutilPromCounter(t, telemetry.EncryptionErrorsTotal.WithLabelValues(op, "stream_failed"))
	if after-before != 1 {
		t.Errorf("counter delta = %v, want 1", after-before)
	}
}

// TestStreamMetricReader_NoIncrementOnEOF asserts the wrapper does NOT
// fire the counter on a clean EOF, the natural end of a healthy stream.
func TestStreamMetricReader_NoIncrementOnEOF(t *testing.T) {
	const op = "decrypt"
	before := testutilPromCounter(t, telemetry.EncryptionErrorsTotal.WithLabelValues(op, "stream_failed"))

	wrapped := withStreamMetric(strings.NewReader("hello"), op)
	if _, err := io.ReadAll(wrapped); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	after := testutilPromCounter(t, telemetry.EncryptionErrorsTotal.WithLabelValues(op, "stream_failed"))
	if after-before != 0 {
		t.Errorf("counter delta = %v, want 0 on clean EOF", after-before)
	}
}

// testutilPromCounter reads the current value of a Prometheus counter.
func testutilPromCounter(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}
