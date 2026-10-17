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

	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
)

// newTestMultipartManager builds a minimal Manager suitable for
// exercising stream helpers in isolation. The core is zero-valued so
// WithTimeout is a no-op; pass an encryptor to enable the decrypt branch.
func newTestMultipartManager(enc *encryption.Encryptor) *Manager {
	return &Manager{
		core:      infra.New(&infra.Config{}),
		encryptor: enc,
	}
}

// preloadPart stores body bytes at the multipart part key for the given
// upload + part number, returning the key for assertions.
func preloadPart(be *backendtest.InMemory, uploadID string, partNum int, body []byte) string {
	key := multipartPartKey(uploadID, partNum)
	be.Objects[key] = backendtest.Object{Data: body, ContentType: "application/octet-stream", ETag: "x"}
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
	be := backendtest.NewInMemory()
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
	be := backendtest.NewInMemory()
	be.GetErr = errors.New("backend down")

	var buf bytes.Buffer
	part := &core.MultipartPart{PartNumber: 7}
	err := mp.streamOnePart(context.Background(), be, &buf, "upload-x", part)
	if err == nil {
		t.Fatal("expected error from failing backend")
	}
	if !strings.Contains(err.Error(), "failed to read part 7") {
		t.Errorf("error missing part number: %v", err)
	}
	if !errors.Is(err, be.GetErr) {
		t.Errorf("error chain missing backend error: %v", err)
	}
}

// TestStreamOnePart_BodyReadErrorWrapsPartNumber verifies a body that fails
// on its first read surfaces while the envelope is inspected, naming the part
// and keeping the backend error in the chain.
func TestStreamOnePart_BodyReadErrorWrapsPartNumber(t *testing.T) {
	t.Parallel()
	mp := newTestMultipartManager(nil)
	be := backendtest.NewInMemory()
	const uploadID = "upload-2"
	preloadPart(be, uploadID, 3, []byte("ignored"))
	be.GetReadErr = errors.New("connection reset")

	var buf bytes.Buffer
	part := &core.MultipartPart{PartNumber: 3}
	err := mp.streamOnePart(context.Background(), be, &buf, uploadID, part)
	if err == nil {
		t.Fatal("expected error from failing read")
	}
	if !strings.Contains(err.Error(), "open part 3") {
		t.Errorf("error missing part number / phase: %v", err)
	}
	if !errors.Is(err, be.GetReadErr) {
		t.Errorf("error chain missing read error: %v", err)
	}
}

// sealPart encrypts plain under enc and returns the stored ciphertext and the
// part row that describes it.
func sealPart(t *testing.T, enc *encryption.Encryptor, partNumber int, plain []byte) ([]byte, *core.MultipartPart) {
	t.Helper()
	res, err := enc.Encrypt(context.Background(), bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	return ciphertext, &core.MultipartPart{
		PartNumber: partNumber,
		SizeBytes:  int64(len(ciphertext)),
		StoredForm: core.StoredForm{
			Encrypted:     true,
			EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
			KeyID:         res.KeyID,
			PlaintextSize: int64(len(plain)),
		},
	}
}

// TestStreamOnePart_FlagMismatchFailsAssembly verifies a part whose row and
// stored bytes disagree about encryption fails in both directions rather than
// assembling ciphertext as plaintext or plaintext as ciphertext.
func TestStreamOnePart_FlagMismatchFailsAssembly(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	ciphertext, sealed := sealPart(t, enc, 8, []byte("sealed part payload"))

	cases := map[string]struct {
		stored []byte
		part   *core.MultipartPart
	}{
		"row says plain":  {stored: ciphertext, part: &core.MultipartPart{PartNumber: 8}},
		"row says sealed": {stored: []byte("plaintext bytes on disk"), part: sealed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mp := newTestMultipartManager(enc)
			be := backendtest.NewInMemory()
			preloadPart(be, "upload-mm", 8, tc.stored)

			var buf bytes.Buffer
			err := mp.streamOnePart(context.Background(), be, &buf, "upload-mm", tc.part)
			if !errors.Is(err, core.ErrEncryptionFlagMismatch) {
				t.Fatalf("error = %v, want ErrEncryptionFlagMismatch", err)
			}
			if buf.Len() != 0 {
				t.Errorf("wrote %d bytes, want none", buf.Len())
			}
		})
	}
}

// TestStreamOnePart_EncryptedPartDecrypts verifies the stream one part encrypted part decrypts contract.
// Asserts that Encrypt:.
func TestStreamOnePart_EncryptedPartDecrypts(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	mp := newTestMultipartManager(enc)
	be := backendtest.NewInMemory()

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

// TestStreamOnePart_EncryptedPartWithoutEncryptorFails verifies an encrypted
// part fails assembly when encryption has since been disabled, rather than
// assembling its ciphertext into the object as though it were plaintext.
func TestStreamOnePart_EncryptedPartWithoutEncryptorFails(t *testing.T) {
	t.Parallel()
	ciphertext, part := sealPart(t, newTestEncryptor(t), 2, []byte("opaque part payload"))
	mp := newTestMultipartManager(nil)
	be := backendtest.NewInMemory()
	const uploadID = "upload-noenc"
	preloadPart(be, uploadID, 2, ciphertext)

	var buf bytes.Buffer
	err := mp.streamOnePart(context.Background(), be, &buf, uploadID, part)
	if !errors.Is(err, storedform.ErrNoEncryptor) {
		t.Fatalf("error = %v, want ErrNoEncryptor", err)
	}
	if !strings.Contains(err.Error(), "open part 2") {
		t.Errorf("error missing part number: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %d bytes of ciphertext, want none", buf.Len())
	}
}

// TestStreamOnePart_BadEncryptionKeyWrapsPartNumber verifies a part whose
// recorded key data cannot be unpacked fails naming the part and the phase.
func TestStreamOnePart_BadEncryptionKeyWrapsPartNumber(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	mp := newTestMultipartManager(enc)
	be := backendtest.NewInMemory()
	const uploadID = "upload-badkey"
	ciphertext, part := sealPart(t, enc, 4, []byte("anything"))
	preloadPart(be, uploadID, 4, ciphertext)

	// EncryptionKey too short -> UnpackKeyData fails.
	part.EncryptionKey = []byte("tiny")

	var buf bytes.Buffer
	err := mp.streamOnePart(context.Background(), be, &buf, uploadID, part)
	if err == nil {
		t.Fatal("expected error for malformed encryption key")
	}
	if !strings.Contains(err.Error(), "unpack part 4 key") {
		t.Errorf("error missing phase / part number: %v", err)
	}
}

// TestStreamOnePart_DecryptErrorWrapsPartNumber verifies an envelope that
// does not open under the part's key fails naming the part, and is not
// mistaken for a flag mismatch.
func TestStreamOnePart_DecryptErrorWrapsPartNumber(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	mp := newTestMultipartManager(enc)
	be := backendtest.NewInMemory()

	// The stored bytes are a real envelope sealed under another data key, so
	// the header check passes and the decrypt itself fails.
	_, part := sealPart(t, enc, 6, []byte("payload whose key the row keeps"))
	other, _ := sealPart(t, enc, 6, []byte("payload whose bytes the backend keeps"))

	const uploadID = "upload-decryptfail"
	preloadPart(be, uploadID, 6, other)

	var buf bytes.Buffer
	streamErr := mp.streamOnePart(context.Background(), be, &buf, uploadID, part)
	if streamErr == nil {
		t.Fatal("expected decrypt failure")
	}
	if !strings.Contains(streamErr.Error(), "part 6") {
		t.Errorf("error missing part number: %v", streamErr)
	}
	if errors.Is(streamErr, core.ErrEncryptionFlagMismatch) {
		t.Errorf("error = %v, want a decrypt failure, not a flag mismatch", streamErr)
	}
}

// TestSumPlaintextSize verifies the assembled size counts each part as the
// bytes the client uploaded: the stored size for a plaintext part and the
// plaintext size for an encrypted one.
func TestSumPlaintextSize(t *testing.T) {
	t.Parallel()
	parts := []core.MultipartPart{
		{PartNumber: 1, SizeBytes: 100},
		{PartNumber: 2, SizeBytes: 164, StoredForm: core.StoredForm{Encrypted: true, PlaintextSize: 100}},
		{PartNumber: 3, SizeBytes: 7},
	}
	if got := sumPlaintextSize(parts); got != 207 {
		t.Errorf("sumPlaintextSize = %d, want 207", got)
	}
	if got := sumPlaintextSize(nil); got != 0 {
		t.Errorf("sumPlaintextSize(nil) = %d, want 0", got)
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
	be := backendtest.NewInMemory()
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
	be := backendtest.NewInMemory()
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
	be := backendtest.NewInMemory()
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
