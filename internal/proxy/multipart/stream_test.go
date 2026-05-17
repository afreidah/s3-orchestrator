// -------------------------------------------------------------------------------
// Multipart Stream Helpers - Unit Tests
//
// Author: Alex Freidah
//
// Tests for streamPartsThroughPipe and streamOnePart, the helpers that
// reassemble multipart uploads. Covers plaintext and encrypted parts,
// per-part error wrapping, and end-to-end pipe semantics.
// -------------------------------------------------------------------------------

package multipart

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// newTestMultipartManager builds a minimal Manager suitable for
// exercising stream helpers in isolation. The Core is zero-valued so
// WithTimeout is a no-op; pass an encryptor to enable the decrypt branch.
func newTestMultipartManager(enc *encryption.Encryptor) *Manager {
	return &Manager{
		Core:      infra.New(&infra.Config{}),
		encryptor: enc,
	}
}

// preloadPart stores body bytes at the multipart part key for the given
// upload + part number, returning the key for assertions.
func preloadPart(be *mockBackend, uploadID string, partNum int, body []byte) string {
	key := multipartPartKey(uploadID, partNum)
	be.objects[key] = mockObject{data: body, contentType: "application/octet-stream", etag: "x"}
	return key
}

// -------------------------------------------------------------------------
// streamOnePart
// -------------------------------------------------------------------------

// TestStreamOnePart_PlaintextCopiesBytes verifies the stream one part plaintext copies bytes contract.
// Asserts that streamOnePart:.
func TestStreamOnePart_PlaintextCopiesBytes(t *testing.T) {
	t.Parallel()
	mp := newTestMultipartManager(nil)
	be := newMockBackend()
	const uploadID = "upload-1"
	preloadPart(be, uploadID, 1, []byte("plaintext part 1 contents"))

	var buf bytes.Buffer
	part := &core.MultipartPart{PartNumber: 1, SizeBytes: 25}
	if err := mp.streamOnePart(context.Background(), be, &buf, uploadID, part); err != nil {
		t.Fatalf("streamOnePart: %v", err)
	}
	if got := buf.String(); got != "plaintext part 1 contents" {
		t.Errorf("unexpected output: %q", got)
	}
}

// TestStreamOnePart_GetObjectErrorWrapsPartNumber verifies the stream one part get object error wraps part number contract.
// Asserts that error missing part number:.
func TestStreamOnePart_GetObjectErrorWrapsPartNumber(t *testing.T) {
	t.Parallel()
	mp := newTestMultipartManager(nil)
	be := newMockBackend()
	be.getErr = errors.New("backend down")

	var buf bytes.Buffer
	part := &core.MultipartPart{PartNumber: 7}
	err := mp.streamOnePart(context.Background(), be, &buf, "upload-x", part)
	if err == nil {
		t.Fatal("expected error from failing backend")
	}
	if !strings.Contains(err.Error(), "failed to read part 7") {
		t.Errorf("error missing part number: %v", err)
	}
	if !errors.Is(err, be.getErr) {
		t.Errorf("error chain missing backend error: %v", err)
	}
}

// TestStreamOnePart_BodyReadErrorWrapsPartNumber verifies the stream one part body read error wraps part number contract.
// Asserts that error missing part number / phase:.
func TestStreamOnePart_BodyReadErrorWrapsPartNumber(t *testing.T) {
	t.Parallel()
	mp := newTestMultipartManager(nil)
	be := newMockBackend()
	const uploadID = "upload-2"
	preloadPart(be, uploadID, 3, []byte("ignored"))
	be.getReadErr = errors.New("connection reset")

	var buf bytes.Buffer
	part := &core.MultipartPart{PartNumber: 3}
	err := mp.streamOnePart(context.Background(), be, &buf, uploadID, part)
	if err == nil {
		t.Fatal("expected error from failing read")
	}
	if !strings.Contains(err.Error(), "failed to stream part 3") {
		t.Errorf("error missing part number / phase: %v", err)
	}
}

// TestStreamOnePart_EncryptedPartDecrypts verifies the stream one part encrypted part decrypts contract.
// Asserts that Encrypt:.
func TestStreamOnePart_EncryptedPartDecrypts(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	mp := newTestMultipartManager(enc)
	be := newMockBackend()

	// Encrypt some plaintext to populate a fake stored part.
	plain := []byte("encrypted part plaintext payload")
	res, err := enc.Encrypt(context.Background(), bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	const uploadID = "upload-enc"
	preloadPart(be, uploadID, 5, ciphertext)
	part := &core.MultipartPart{
		PartNumber:    5,
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
		KeyID:         res.KeyID,
		PlaintextSize: int64(len(plain)),
	}

	var buf bytes.Buffer
	if err := mp.streamOnePart(context.Background(), be, &buf, uploadID, part); err != nil {
		t.Fatalf("streamOnePart: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), plain) {
		t.Errorf("plaintext mismatch: got %q, want %q", buf.String(), plain)
	}
}

// TestStreamOnePart_EncryptedPartWithoutEncryptorPassesCiphertext verifies the stream one part encrypted part without encryptor passes ciphertext contract.
// Asserts that streamOnePart:.
func TestStreamOnePart_EncryptedPartWithoutEncryptorPassesCiphertext(t *testing.T) {
	t.Parallel()
	// Existing behavior: when part.Encrypted is true but mp.encryptor is nil
	// (encryption disabled mid-upload), the helper writes ciphertext as-is.
	mp := newTestMultipartManager(nil)
	be := newMockBackend()
	const uploadID = "upload-noenc"
	ciphertext := []byte("opaque ciphertext bytes")
	preloadPart(be, uploadID, 2, ciphertext)

	part := &core.MultipartPart{PartNumber: 2, Encrypted: true}

	var buf bytes.Buffer
	if err := mp.streamOnePart(context.Background(), be, &buf, uploadID, part); err != nil {
		t.Fatalf("streamOnePart: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), ciphertext) {
		t.Errorf("expected ciphertext passthrough, got %q", buf.String())
	}
}

// TestStreamOnePart_BadEncryptionKeyWrapsPartNumber verifies the stream one part bad encryption key wraps part number contract.
// Asserts that error missing phase / part number:.
func TestStreamOnePart_BadEncryptionKeyWrapsPartNumber(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	mp := newTestMultipartManager(enc)
	be := newMockBackend()
	const uploadID = "upload-badkey"
	preloadPart(be, uploadID, 4, []byte("anything"))

	// EncryptionKey too short -> UnpackKeyData fails.
	part := &core.MultipartPart{PartNumber: 4, Encrypted: true, EncryptionKey: []byte("tiny")}

	var buf bytes.Buffer
	err := mp.streamOnePart(context.Background(), be, &buf, uploadID, part)
	if err == nil {
		t.Fatal("expected error for malformed encryption key")
	}
	if !strings.Contains(err.Error(), "unpack part 4 key") {
		t.Errorf("error missing phase / part number: %v", err)
	}
}

// TestStreamOnePart_DecryptErrorWrapsPartNumber verifies the stream one part decrypt error wraps part number contract.
// Asserts that Encrypt:.
func TestStreamOnePart_DecryptErrorWrapsPartNumber(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	mp := newTestMultipartManager(enc)
	be := newMockBackend()

	// Encrypt one payload to mint valid key data, but store garbage as the
	// "ciphertext" so Decrypt fails on parse/verify.
	res, err := enc.Encrypt(context.Background(), bytes.NewReader([]byte("x")), 1)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	_, _ = io.ReadAll(res.Body)

	const uploadID = "upload-decryptfail"
	preloadPart(be, uploadID, 6, []byte("not-real-ciphertext-bytes"))
	part := &core.MultipartPart{
		PartNumber:    6,
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
		KeyID:         res.KeyID,
	}

	var buf bytes.Buffer
	streamErr := mp.streamOnePart(context.Background(), be, &buf, uploadID, part)
	if streamErr == nil {
		t.Fatal("expected decrypt failure")
	}
	if !strings.Contains(streamErr.Error(), "decrypt part 6") {
		t.Errorf("error missing phase / part number: %v", streamErr)
	}
}

// -------------------------------------------------------------------------
// streamPartsThroughPipe
// -------------------------------------------------------------------------

// TestStreamPartsThroughPipe_ConcatenatesInOrder verifies the stream parts through pipe concatenates in order contract.
// Asserts that read pipe:.
func TestStreamPartsThroughPipe_ConcatenatesInOrder(t *testing.T) {
	t.Parallel()
	mp := newTestMultipartManager(nil)
	be := newMockBackend()
	const uploadID = "upload-concat"
	preloadPart(be, uploadID, 1, []byte("alpha-"))
	preloadPart(be, uploadID, 2, []byte("beta-"))
	preloadPart(be, uploadID, 3, []byte("gamma"))

	parts := []core.MultipartPart{
		{PartNumber: 1, SizeBytes: 6},
		{PartNumber: 2, SizeBytes: 5},
		{PartNumber: 3, SizeBytes: 5},
	}

	pr, cancel := mp.streamPartsThroughPipe(context.Background(), be, uploadID, parts)
	defer cancel()

	got, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if string(got) != "alpha-beta-gamma" {
		t.Errorf("unexpected concatenation: %q", got)
	}
}

// TestStreamPartsThroughPipe_PropagatesPartFailure verifies the stream parts through pipe propagates part failure contract.
// Asserts that error missing phase / part number:.
func TestStreamPartsThroughPipe_PropagatesPartFailure(t *testing.T) {
	t.Parallel()
	mp := newTestMultipartManager(nil)
	be := newMockBackend()
	const uploadID = "upload-partfail"
	preloadPart(be, uploadID, 1, []byte("ok-1"))
	// Part 2 deliberately not preloaded  -  backend returns NotFound.
	preloadPart(be, uploadID, 3, []byte("ok-3"))

	parts := []core.MultipartPart{
		{PartNumber: 1, SizeBytes: 4},
		{PartNumber: 2, SizeBytes: 4},
		{PartNumber: 3, SizeBytes: 4},
	}

	pr, cancel := mp.streamPartsThroughPipe(context.Background(), be, uploadID, parts)
	defer cancel()

	_, err := io.ReadAll(pr)
	if err == nil {
		t.Fatal("expected error for missing part")
	}
	if !strings.Contains(err.Error(), "failed to read part 2") {
		t.Errorf("error missing phase / part number: %v", err)
	}
}

// TestStreamPartsThroughPipe_MixedEncryptedAndPlain verifies the stream parts through pipe mixed encrypted and plain contract.
// Asserts that Encrypt:.
func TestStreamPartsThroughPipe_MixedEncryptedAndPlain(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	mp := newTestMultipartManager(enc)
	be := newMockBackend()
	const uploadID = "upload-mixed"

	// Part 1: plaintext
	preloadPart(be, uploadID, 1, []byte("PLAIN1-"))

	// Part 2: encrypted
	plain2 := []byte("ENCRYPTED2-")
	res, err := enc.Encrypt(context.Background(), bytes.NewReader(plain2), int64(len(plain2)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct2, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	preloadPart(be, uploadID, 2, ct2)

	// Part 3: plaintext
	preloadPart(be, uploadID, 3, []byte("PLAIN3"))

	parts := []core.MultipartPart{
		{PartNumber: 1, SizeBytes: 7},
		{
			PartNumber:    2,
			Encrypted:     true,
			EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
			KeyID:         res.KeyID,
			PlaintextSize: int64(len(plain2)),
		},
		{PartNumber: 3, SizeBytes: 6},
	}

	pr, cancel := mp.streamPartsThroughPipe(context.Background(), be, uploadID, parts)
	defer cancel()

	got, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if string(got) != "PLAIN1-ENCRYPTED2-PLAIN3" {
		t.Errorf("unexpected output: %q", got)
	}
}
