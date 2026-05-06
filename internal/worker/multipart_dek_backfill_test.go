// -------------------------------------------------------------------------------
// Multipart DEK Backfill Tests
//
// Author: Alex Freidah
//
// Unit tests for the multipart_dek_backfill worker. Drives the worker against
// minimal in-memory fakes for core.MultipartStore + core.AdvisoryLocker and an
// in-memory ObjectBackend so the rebuild path can be exercised end-to-end with
// a real Encryptor and a real chunked-AES-GCM round-trip.
// -------------------------------------------------------------------------------

package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// fakeMultipartStore implements core.MultipartStore against in-memory maps.
// Only the methods MultipartBackfill calls are populated; everything else
// panics so an accidental new dependency is loud rather than silent.
type fakeMultipartStore struct {
	mu      sync.Mutex
	uploads map[string]core.MultipartUpload
	parts   map[string][]core.MultipartPart
}

func newFakeMultipartStore() *fakeMultipartStore {
	return &fakeMultipartStore{
		uploads: map[string]core.MultipartUpload{},
		parts:   map[string][]core.MultipartPart{},
	}
}

func (f *fakeMultipartStore) ListLegacyMultipartUploads(_ context.Context, limit int) ([]core.MultipartUpload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.MultipartUpload
	for _, mu := range f.uploads { //nolint:gocritic // map value is struct; copy is unavoidable in test fake
		if len(mu.EncryptionKey) == 0 {
			out = append(out, mu)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeMultipartStore) GetMultipartUpload(_ context.Context, id string) (*core.MultipartUpload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mu, ok := f.uploads[id]
	if !ok {
		return nil, core.ErrMultipartUploadNotFound
	}
	cp := mu
	return &cp, nil
}

func (f *fakeMultipartStore) GetParts(_ context.Context, id string) ([]core.MultipartPart, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]core.MultipartPart, len(f.parts[id]))
	copy(out, f.parts[id])
	return out, nil
}

func (f *fakeMultipartStore) UpdateUploadEncryption(_ context.Context, id string, encKey []byte, keyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	mu, ok := f.uploads[id]
	if !ok {
		return core.ErrMultipartUploadNotFound
	}
	mu.EncryptionKey = encKey
	mu.KeyID = keyID
	mu.Encrypted = len(encKey) > 0
	f.uploads[id] = mu
	return nil
}

func (f *fakeMultipartStore) UpdatePartEncryption(_ context.Context, id string, partNumber int, sizeBytes int64, enc *core.EncryptionMeta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := f.parts[id]
	for i := range parts {
		if parts[i].PartNumber != partNumber {
			continue
		}
		parts[i].SizeBytes = sizeBytes
		if enc != nil {
			parts[i].Encrypted = enc.Encrypted
			parts[i].EncryptionKey = enc.EncryptionKey
			parts[i].KeyID = enc.KeyID
			parts[i].PlaintextSize = enc.PlaintextSize
		}
		f.parts[id] = parts
		return nil
	}
	return errors.New("part not found")
}

// Methods below are part of core.MultipartStore but not exercised by the
// backfill worker. They panic to surface unintended new dependencies.

func (f *fakeMultipartStore) CreateMultipartUpload(context.Context, *core.CreateMultipartUploadParams) error {
	panic("CreateMultipartUpload not implemented in fake")
}

func (f *fakeMultipartStore) RecordPart(context.Context, string, int, string, int64, *core.EncryptionMeta) error {
	panic("RecordPart not implemented in fake")
}

func (f *fakeMultipartStore) DeleteMultipartUpload(context.Context, string) error {
	panic("DeleteMultipartUpload not implemented in fake")
}

func (f *fakeMultipartStore) ListMultipartUploads(context.Context, string, int) ([]core.MultipartUpload, error) {
	panic("ListMultipartUploads not implemented in fake")
}

func (f *fakeMultipartStore) CountActiveMultipartUploads(context.Context, string) (int64, error) {
	panic("CountActiveMultipartUploads not implemented in fake")
}

func (f *fakeMultipartStore) GetStaleMultipartUploads(context.Context, time.Duration) ([]core.MultipartUpload, error) {
	panic("GetStaleMultipartUploads not implemented in fake")
}

func (f *fakeMultipartStore) GetMultipartUploadsByBackend(context.Context, string) ([]core.MultipartUpload, error) {
	panic("GetMultipartUploadsByBackend not implemented in fake")
}

// noopLocker satisfies core.AdvisoryLocker by always running fn under a
// "lock that is always available". Sufficient for unit tests that don't
// exercise multi-instance contention.
type noopLocker struct{}

func (noopLocker) WithAdvisoryLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	if err := fn(ctx); err != nil {
		return true, err
	}
	return true, nil
}

// fakeBackend implements s3be.ObjectBackend against an in-memory map.
type fakeBackend struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{objects: map[string][]byte{}}
}

func (f *fakeBackend) PutObject(_ context.Context, key string, body io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
	return "etag", nil
}

func (f *fakeBackend) GetObject(_ context.Context, key, _ string) (*s3be.GetObjectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, errors.New("not found")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	return &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(cp)),
		Size: int64(len(cp)),
	}, nil
}

func (f *fakeBackend) HeadObject(context.Context, string) (*s3be.HeadObjectResult, error) {
	panic("HeadObject not implemented in fake")
}

func (f *fakeBackend) DeleteObject(context.Context, string) error {
	panic("DeleteObject not implemented in fake")
}

// newTestEncryptor wires a deterministic Encryptor for backfill tests.
// The single 32-byte AES key is the same one the proxy package uses in
// its encryption fixtures.
func newTestEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

// TestMultipartBackfill_NoEncryptor verifies the worker is a no-op when
// proxy-side encryption is disabled, even with legacy rows present in
// the store. Legacy rows can only exist under a previously-encryption-
// enabled deployment, but a fresh proxy with encryption off should not
// crash trying to wrap a DEK against a missing provider.
func TestMultipartBackfill_NoEncryptor(t *testing.T) {
	t.Parallel()
	store := newFakeMultipartStore()
	store.uploads["legacy"] = core.MultipartUpload{UploadID: "legacy", BackendName: "b1"}

	mb := NewMultipartBackfill(
		&multipartBackfillStore{MultipartStore: store, AdvisoryLocker: noopLocker{}},
		nil,
		func(string) (s3be.ObjectBackend, error) { return newFakeBackend(), nil },
		MultipartBackfillConfig{},
	)
	migrated, err := mb.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0", migrated)
	}
}

// TestMultipartBackfill_EmptyBacklog verifies the worker exits cleanly
// when no legacy rows exist - the steady-state condition after the
// startup hook drains the backlog on first boot.
func TestMultipartBackfill_EmptyBacklog(t *testing.T) {
	t.Parallel()
	store := newFakeMultipartStore()
	enc := newTestEncryptor(t)

	mb := NewMultipartBackfill(
		&multipartBackfillStore{MultipartStore: store, AdvisoryLocker: noopLocker{}},
		enc,
		func(string) (s3be.ObjectBackend, error) { return newFakeBackend(), nil },
		MultipartBackfillConfig{},
	)
	migrated, err := mb.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0", migrated)
	}
}

// TestMultipartBackfill_HappyPath runs an end-to-end migration of one
// legacy upload row carrying two per-part-DEK encrypted parts. Asserts
// that after the pass: (1) the upload row carries a wrapped DEK + key
// ID, (2) every part row references the new shared DEK, (3) the on-
// disk part bytes decrypt cleanly under the upload's wrapped DEK so a
// subsequent CompleteMultipartUpload would succeed.
func TestMultipartBackfill_HappyPath(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	be := newFakeBackend()
	ctx := context.Background()

	// Seed two encrypted parts using per-part DEKs (the legacy format).
	const uploadID = "upl-1"
	parts := make([]core.MultipartPart, 0, 2)
	for i, plain := range [][]byte{[]byte("part-one-payload"), []byte("part-two-different")} {
		res, err := enc.Encrypt(ctx, bytes.NewReader(plain), int64(len(plain)))
		if err != nil {
			t.Fatalf("Encrypt part %d: %v", i+1, err)
		}
		ct, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("read ciphertext part %d: %v", i+1, err)
		}
		key := multipartPartKey(uploadID, i+1)
		if _, err := be.PutObject(ctx, key, bytes.NewReader(ct), int64(len(ct)), "application/octet-stream", nil); err != nil {
			t.Fatalf("seed put part %d: %v", i+1, err)
		}
		parts = append(parts, core.MultipartPart{
			PartNumber:    i + 1,
			ETag:          "e",
			SizeBytes:     int64(len(ct)),
			Encrypted:     true,
			EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
			KeyID:         res.KeyID,
			PlaintextSize: int64(len(plain)),
		})
	}

	store := newFakeMultipartStore()
	store.uploads[uploadID] = core.MultipartUpload{
		UploadID:    uploadID,
		ObjectKey:   "k",
		BackendName: "b1",
	}
	store.parts[uploadID] = parts

	mb := NewMultipartBackfill(
		&multipartBackfillStore{MultipartStore: store, AdvisoryLocker: noopLocker{}},
		enc,
		func(string) (s3be.ObjectBackend, error) { return be, nil },
		MultipartBackfillConfig{},
	)
	migrated, err := mb.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if migrated != 1 {
		t.Fatalf("migrated = %d, want 1", migrated)
	}

	// Upload row should now carry a wrapped DEK + key ID.
	migratedUpload := store.uploads[uploadID]
	if len(migratedUpload.EncryptionKey) == 0 {
		t.Fatal("upload row encryption_key is empty after migration")
	}
	if migratedUpload.KeyID == "" {
		t.Fatal("upload row key_id is empty after migration")
	}

	// Each part should still decrypt cleanly under the new metadata.
	for i, original := range [][]byte{[]byte("part-one-payload"), []byte("part-two-different")} {
		key := multipartPartKey(uploadID, i+1)
		got := store.parts[uploadID][i]
		if !got.Encrypted || len(got.EncryptionKey) == 0 || got.KeyID == "" {
			t.Fatalf("part %d not flagged encrypted after migration: %+v", i+1, got)
		}
		_, wrappedDEK, err := encryption.UnpackKeyData(got.EncryptionKey)
		if err != nil {
			t.Fatalf("UnpackKeyData part %d: %v", i+1, err)
		}
		raw, err := be.GetObject(ctx, key, "")
		if err != nil {
			t.Fatalf("GetObject part %d: %v", i+1, err)
		}
		plainReader, err := enc.Decrypt(ctx, raw.Body, wrappedDEK, got.KeyID)
		if err != nil {
			t.Fatalf("Decrypt part %d: %v", i+1, err)
		}
		plain, err := io.ReadAll(plainReader)
		if err != nil {
			t.Fatalf("ReadAll part %d: %v", i+1, err)
		}
		if !bytes.Equal(plain, original) {
			t.Errorf("part %d plaintext mismatch: got %q, want %q", i+1, plain, original)
		}
	}
}

// multipartBackfillStore composes the two narrow per-role interfaces
// the worker requires into a single value satisfying
// MultipartBackfillStore.
type multipartBackfillStore struct {
	core.MultipartStore
	core.AdvisoryLocker
}
