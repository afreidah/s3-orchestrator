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

	// Error-injection knobs. Tests set these to drive specific
	// failure branches in the worker.
	errList               error
	errGet                error
	errGetParts           error
	errUpdateUpload       error
	errUpdatePart         error
	notFoundOnGet         bool
	getReturnsMigrated    bool // Get returns a row with encryption_key already set, simulating a concurrent migrate
	clearOnList           bool // make ListLegacy return [] after first call (avoids hot-loop)
	listCallCount         int
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
	f.listCallCount++
	if f.errList != nil {
		return nil, f.errList
	}
	if f.clearOnList && f.listCallCount > 1 {
		return nil, nil
	}
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
	if f.errGet != nil {
		return nil, f.errGet
	}
	if f.notFoundOnGet {
		return nil, core.ErrMultipartUploadNotFound
	}
	mu, ok := f.uploads[id]
	if !ok {
		return nil, core.ErrMultipartUploadNotFound
	}
	cp := mu
	if f.getReturnsMigrated {
		cp.Encrypted = true
		cp.EncryptionKey = []byte("already-migrated-by-peer")
		cp.KeyID = "peer-key"
	}
	return &cp, nil
}

func (f *fakeMultipartStore) GetParts(_ context.Context, id string) ([]core.MultipartPart, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errGetParts != nil {
		return nil, f.errGetParts
	}
	out := make([]core.MultipartPart, len(f.parts[id]))
	copy(out, f.parts[id])
	return out, nil
}

func (f *fakeMultipartStore) UpdateUploadEncryption(_ context.Context, id string, encKey []byte, keyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errUpdateUpload != nil {
		return f.errUpdateUpload
	}
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
	if f.errUpdatePart != nil {
		return f.errUpdatePart
	}
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

// programmableLocker drives both lock branches the worker can take:
// acquired=false (another caller holds it) and a non-nil error from
// the locker itself (e.g., DB outage).
type programmableLocker struct {
	acquired bool
	err      error
}

func (p programmableLocker) WithAdvisoryLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	if p.err != nil {
		return false, p.err
	}
	if !p.acquired {
		return false, nil
	}
	if err := fn(ctx); err != nil {
		return true, err
	}
	return true, nil
}

// fakeBackend implements s3be.ObjectBackend against an in-memory map.
type fakeBackend struct {
	mu      sync.Mutex
	objects map[string][]byte

	errPut error
	errGet error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{objects: map[string][]byte{}}
}

func (f *fakeBackend) PutObject(_ context.Context, key string, body io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
	f.mu.Lock()
	if f.errPut != nil {
		err := f.errPut
		f.mu.Unlock()
		return "", err
	}
	f.mu.Unlock()
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
	if f.errGet != nil {
		return nil, f.errGet
	}
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

	const uploadID = "upl-1"
	plaintexts := [][]byte{[]byte("part-one-payload"), []byte("part-two-different")}
	parts := seedLegacyParts(t, ctx, enc, be, uploadID, plaintexts)

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

	migratedUpload := store.uploads[uploadID]
	if len(migratedUpload.EncryptionKey) == 0 {
		t.Fatal("upload row encryption_key is empty after migration")
	}
	if migratedUpload.KeyID == "" {
		t.Fatal("upload row key_id is empty after migration")
	}

	asserter := partAssertion{enc: enc, be: be}
	for i, original := range plaintexts {
		asserter.assertPartDecrypts(t, uploadID, i+1, &store.parts[uploadID][i], original)
	}
}

// seedLegacyParts encrypts each plaintext under a fresh per-part DEK,
// stores the ciphertext on the fake backend at the canonical multipart
// part key, and returns the matching MultipartPart rows the worker
// would see in the store. Mirrors the on-disk shape of an upload that
// was created before migration 00010.
func seedLegacyParts(t *testing.T, ctx context.Context, enc *encryption.Encryptor, be *fakeBackend, uploadID string, plaintexts [][]byte) []core.MultipartPart {
	t.Helper()
	parts := make([]core.MultipartPart, 0, len(plaintexts))
	for i, plain := range plaintexts {
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
	return parts
}

// assertPartDecrypts fetches the part's ciphertext from the backend,
// decrypts it under the metadata recorded on the part row after
// migration, and verifies the plaintext matches the seed. A failure
// here means the on-disk bytes drifted from the row's DEK reference.
// partAssertion bundles the test fixtures assertPartDecrypts needs
// so the helper signature stays under the 7-parameter limit.
type partAssertion struct {
	enc *encryption.Encryptor
	be  *fakeBackend
}

// assertPartDecrypts fetches the part's ciphertext from the backend,
// decrypts it under the metadata recorded on the part row after
// migration, and verifies the plaintext matches the seed. A failure
// here means the on-disk bytes drifted from the row's DEK reference.
func (a partAssertion) assertPartDecrypts(t *testing.T, uploadID string, partNumber int, got *core.MultipartPart, want []byte) {
	t.Helper()
	ctx := context.Background()
	if !got.Encrypted || len(got.EncryptionKey) == 0 || got.KeyID == "" {
		t.Fatalf("part %d not flagged encrypted after migration: %+v", partNumber, got)
	}
	_, wrappedDEK, err := encryption.UnpackKeyData(got.EncryptionKey)
	if err != nil {
		t.Fatalf("UnpackKeyData part %d: %v", partNumber, err)
	}
	raw, err := a.be.GetObject(ctx, multipartPartKey(uploadID, partNumber), "")
	if err != nil {
		t.Fatalf("GetObject part %d: %v", partNumber, err)
	}
	plainReader, err := a.enc.Decrypt(ctx, raw.Body, wrappedDEK, got.KeyID)
	if err != nil {
		t.Fatalf("Decrypt part %d: %v", partNumber, err)
	}
	plain, err := io.ReadAll(plainReader)
	if err != nil {
		t.Fatalf("ReadAll part %d: %v", partNumber, err)
	}
	if !bytes.Equal(plain, want) {
		t.Errorf("part %d plaintext mismatch: got %q, want %q", partNumber, plain, want)
	}
}

// multipartBackfillStore composes the two narrow per-role interfaces
// the worker requires into a single value satisfying
// MultipartBackfillStore.
type multipartBackfillStore struct {
	core.MultipartStore
	core.AdvisoryLocker
}

// newOneLegacyUploadFixture seeds the fake store + backend with one
// legacy multipart upload carrying one encrypted part. Returned to
// callers so they can drive the worker against a known-good baseline
// and then inject errors via the fake's knobs.
func newOneLegacyUploadFixture(t *testing.T, ctx context.Context, enc *encryption.Encryptor) (*fakeMultipartStore, *fakeBackend, string) {
	t.Helper()
	const uploadID = "upl-fixture"
	be := newFakeBackend()
	parts := seedLegacyParts(t, ctx, enc, be, uploadID, [][]byte{[]byte("payload")})
	store := newFakeMultipartStore()
	store.uploads[uploadID] = core.MultipartUpload{UploadID: uploadID, BackendName: "b1"}
	store.parts[uploadID] = parts
	store.clearOnList = true
	return store, be, uploadID
}

// newBackfillForTest wires a worker against the supplied fakes with
// the smallest config the tests need (default page size, single-
// goroutine concurrency so error counters are deterministic).
func newBackfillForTest(store *fakeMultipartStore, locker core.AdvisoryLocker, enc *encryption.Encryptor, be *fakeBackend, beErr error) *MultipartBackfill {
	return NewMultipartBackfill(
		&multipartBackfillStore{MultipartStore: store, AdvisoryLocker: locker},
		enc,
		func(string) (s3be.ObjectBackend, error) {
			if beErr != nil {
				return nil, beErr
			}
			return be, nil
		},
		MultipartBackfillConfig{},
	)
}

// TestMultipartBackfill_ListError verifies a failure from
// ListLegacyMultipartUploads aborts the run with a wrapped error.
func TestMultipartBackfill_ListError(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	store := newFakeMultipartStore()
	store.errList = errors.New("db down")
	mb := newBackfillForTest(store, noopLocker{}, enc, newFakeBackend(), nil)
	migrated, err := mb.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0", migrated)
	}
}

// TestMultipartBackfill_ContextCancelled verifies the loop exits
// before issuing the first list when ctx is already cancelled.
func TestMultipartBackfill_ContextCancelled(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	store := newFakeMultipartStore()
	mb := newBackfillForTest(store, noopLocker{}, enc, newFakeBackend(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	migrated, err := mb.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0", migrated)
	}
}

// TestMultipartBackfill_LockNotAcquired verifies the worker treats a
// declined lock as a no-op (the runtime path holds it; the row will
// be retried on the next pass) and that the upload row stays in
// legacy state.
func TestMultipartBackfill_LockNotAcquired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	store, be, uploadID := newOneLegacyUploadFixture(t, ctx, enc)
	mb := newBackfillForTest(store, programmableLocker{acquired: false}, enc, be, nil)
	if _, err := mb.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(store.uploads[uploadID].EncryptionKey) != 0 {
		t.Errorf("upload row was stamped despite lock decline")
	}
}

// TestMultipartBackfill_LockError verifies a locker error surfaces as
// a per-row failure (logged and counted) rather than aborting the
// whole run; subsequent rows still get a chance.
func TestMultipartBackfill_LockError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	store, be, _ := newOneLegacyUploadFixture(t, ctx, enc)
	mb := newBackfillForTest(store, programmableLocker{err: errors.New("lock acquire failed")}, enc, be, nil)
	migrated, err := mb.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0 (lock error)", migrated)
	}
}

// TestMultipartBackfill_GetUploadError covers the rebuildLocked branch
// where re-reading the upload row fails with a non-NotFound error.
func TestMultipartBackfill_GetUploadError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	store, be, _ := newOneLegacyUploadFixture(t, ctx, enc)
	store.errGet = errors.New("transient db error")
	mb := newBackfillForTest(store, noopLocker{}, enc, be, nil)
	migrated, err := mb.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0 (get failed)", migrated)
	}
}

// TestMultipartBackfill_RowDisappeared covers the rebuildLocked branch
// where the row was removed (Complete or Abort) between listing and
// lock acquisition. The worker treats it as a successful no-op.
func TestMultipartBackfill_RowDisappeared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	store, be, _ := newOneLegacyUploadFixture(t, ctx, enc)
	store.notFoundOnGet = true
	mb := newBackfillForTest(store, noopLocker{}, enc, be, nil)
	migrated, err := mb.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// No-op rebuild still counts as a successful pass for that row.
	if migrated != 1 {
		t.Errorf("migrated = %d, want 1 (no-op success)", migrated)
	}
}

// TestMultipartBackfill_AlreadyMigrated covers the rebuildLocked branch
// where another instance migrated the row between listing and lock
// acquisition: List sees it as legacy, but Get under the lock sees
// it already stamped.
func TestMultipartBackfill_AlreadyMigrated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	store, be, _ := newOneLegacyUploadFixture(t, ctx, enc)
	store.getReturnsMigrated = true
	mb := newBackfillForTest(store, noopLocker{}, enc, be, nil)
	if _, err := mb.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

// TestMultipartBackfill_BackendResolveError covers the branch where
// the configured backend disappears between upload create and the
// backfill pass.
func TestMultipartBackfill_BackendResolveError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	store, _, _ := newOneLegacyUploadFixture(t, ctx, enc)
	mb := newBackfillForTest(store, noopLocker{}, enc, nil, errors.New("backend gone"))
	migrated, err := mb.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0 (backend resolve failed)", migrated)
	}
}

// TestMultipartBackfill_GetPartsError covers the GetParts failure
// branch in rebuildLocked.
func TestMultipartBackfill_GetPartsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	store, be, _ := newOneLegacyUploadFixture(t, ctx, enc)
	store.errGetParts = errors.New("parts query failed")
	mb := newBackfillForTest(store, noopLocker{}, enc, be, nil)
	migrated, err := mb.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0", migrated)
	}
}

// TestMultipartBackfill_PartGetObjectError covers the GetObject error
// path inside rebuildPart.
func TestMultipartBackfill_PartGetObjectError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	store, be, _ := newOneLegacyUploadFixture(t, ctx, enc)
	be.errGet = errors.New("backend get failed")
	mb := newBackfillForTest(store, noopLocker{}, enc, be, nil)
	migrated, err := mb.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0", migrated)
	}
}

// TestMultipartBackfill_PartPutObjectError covers the PutObject error
// path. Forces the backfill to abandon the upload after re-encryption
// completes but before the row update lands.
func TestMultipartBackfill_PartPutObjectError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	store, be, _ := newOneLegacyUploadFixture(t, ctx, enc)
	be.errPut = errors.New("backend put failed")
	mb := newBackfillForTest(store, noopLocker{}, enc, be, nil)
	migrated, err := mb.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0", migrated)
	}
}

// failingKeyProvider returns errors from WrapDEK + UnwrapDEK so
// tests can drive the encryption-error paths in the backfill
// worker (GenerateAndWrapDEK failure inside rebuildLocked).
type failingKeyProvider struct{}

func (failingKeyProvider) WrapDEK(_ context.Context, _ []byte) ([]byte, string, error) {
	return nil, "", errors.New("simulated wrap failure")
}
func (failingKeyProvider) UnwrapDEK(_ context.Context, _ []byte, _ string) ([]byte, error) {
	return nil, errors.New("simulated unwrap failure")
}
func (failingKeyProvider) KeyID() string { return "fail-0" }

// TestMultipartBackfill_WrapDEKError covers the rebuildLocked
// branch where GenerateAndWrapDEK fails. The worker logs and skips
// the row; the upload row stays in legacy state.
func TestMultipartBackfill_WrapDEKError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	failEnc, err := encryption.NewEncryptor(failingKeyProvider{}, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	store := newFakeMultipartStore()
	store.uploads["upl-wrap"] = core.MultipartUpload{UploadID: "upl-wrap", BackendName: "b1"}
	store.parts["upl-wrap"] = []core.MultipartPart{{PartNumber: 1, Encrypted: true}}
	store.clearOnList = true
	mb := newBackfillForTest(store, noopLocker{}, failEnc, newFakeBackend(), nil)
	if _, err := mb.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(store.uploads["upl-wrap"].EncryptionKey) != 0 {
		t.Error("upload row stamped despite wrap failure")
	}
}

// TestMultipartBackfill_PartDecryptError covers the rebuildPart
// branch where the part ciphertext fails to decrypt (the wrapped
// DEK on the part row is well-formed but does not match the bytes
// on the backend). Surfaces a per-row error and leaves the upload
// in legacy state for the operator to abort.
func TestMultipartBackfill_PartDecryptError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	be := newFakeBackend()
	const uploadID = "upl-decrypt-err"
	// Stash garbage at the part key. decryptLegacyPart will read
	// it and the AEAD auth check will reject the bytes.
	partKey := multipartPartKey(uploadID, 1)
	if _, err := be.PutObject(ctx, partKey, bytes.NewReader([]byte("not-encrypted-bytes")), 0, "", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store := newFakeMultipartStore()
	store.uploads[uploadID] = core.MultipartUpload{UploadID: uploadID, BackendName: "b1"}
	store.parts[uploadID] = []core.MultipartPart{{
		PartNumber:    1,
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(make([]byte, encryption.NonceSize), []byte("not-real-wrapped-dek")),
		KeyID:         "test-0",
	}}
	store.clearOnList = true
	mb := newBackfillForTest(store, noopLocker{}, enc, be, nil)
	if _, err := mb.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(store.uploads[uploadID].EncryptionKey) != 0 {
		t.Error("upload row stamped despite part decrypt failure")
	}
}

// TestMultipartBackfill_UpdatePartEncryptionError covers the
// UpdatePartEncryption failure path: bytes were rewritten on the
// backend but the row update lost. The worker must surface the error.
func TestMultipartBackfill_UpdatePartEncryptionError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	store, be, _ := newOneLegacyUploadFixture(t, ctx, enc)
	store.errUpdatePart = errors.New("update part failed")
	mb := newBackfillForTest(store, noopLocker{}, enc, be, nil)
	migrated, err := mb.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0", migrated)
	}
}

// TestMultipartBackfill_UpdateUploadEncryptionError covers the final
// commit branch: every part was rebuilt, but stamping the upload row
// fails and leaves the row in legacy state.
func TestMultipartBackfill_UpdateUploadEncryptionError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newTestEncryptor(t)
	store, be, uploadID := newOneLegacyUploadFixture(t, ctx, enc)
	store.errUpdateUpload = errors.New("commit failed")
	mb := newBackfillForTest(store, noopLocker{}, enc, be, nil)
	migrated, err := mb.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if migrated != 0 {
		t.Errorf("migrated = %d, want 0 (commit failed)", migrated)
	}
	if len(store.uploads[uploadID].EncryptionKey) != 0 {
		t.Errorf("upload row stamped despite commit failure")
	}
}

// TestDecryptLegacyPart_Unencrypted covers the branch where a part
// recorded plaintext under a legacy-flagged upload row: bytes are
// streamed through unchanged.
func TestDecryptLegacyPart_Unencrypted(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	want := []byte("plaintext-payload")
	got, err := decryptLegacyPart(context.Background(), enc, bytes.NewReader(want), &core.MultipartPart{Encrypted: false})
	if err != nil {
		t.Fatalf("decryptLegacyPart: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("plaintext mismatch: got %q, want %q", got, want)
	}
}

// TestDecryptLegacyPart_UnpackError covers the branch where the
// part's encryption_key column is shorter than the nonce prefix
// UnpackKeyData expects.
func TestDecryptLegacyPart_UnpackError(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	_, err := decryptLegacyPart(context.Background(), enc, bytes.NewReader(nil), &core.MultipartPart{
		Encrypted:     true,
		EncryptionKey: []byte{0x01, 0x02}, // too short for any nonce + DEK split
		KeyID:         "k",
	})
	if err == nil {
		t.Fatal("expected unpack error, got nil")
	}
}

// TestDecryptLegacyPart_DecryptError covers the branch where the
// wrapped DEK is well-formed but the ciphertext stream cannot be
// decrypted under it.
func TestDecryptLegacyPart_DecryptError(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	// 12 nonce bytes + arbitrary "wrapped" DEK bytes. Will pass
	// UnpackKeyData but fail at the AEAD decrypt step.
	bogusKey := encryption.PackKeyData(make([]byte, encryption.NonceSize), []byte("not-a-real-wrapped-dek"))
	_, err := decryptLegacyPart(context.Background(), enc, bytes.NewReader([]byte("garbage")), &core.MultipartPart{
		Encrypted:     true,
		EncryptionKey: bogusKey,
		KeyID:         "test-0",
	})
	if err == nil {
		t.Fatal("expected decrypt error, got nil")
	}
}

// TestMultipartBackfill_RunPeriodic verifies the periodic loop fires
// at least one Run cycle and exits cleanly when ctx is cancelled.
// Uses an unbuffered signal so the test does not race the ticker.
func TestMultipartBackfill_RunPeriodic(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	store, be, _ := newOneLegacyUploadFixture(t, context.Background(), enc)
	mb := newBackfillForTest(store, noopLocker{}, enc, be, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		mb.RunPeriodic(ctx, 5*time.Millisecond)
		close(done)
	}()

	// Wait for at least one tick to fire by polling the migrated
	// state on the store. Avoids an arbitrary sleep that would race
	// the ticker on slow CI.
	deadline := time.After(2 * time.Second)
	for {
		store.mu.Lock()
		migrated := len(store.uploads) > 0 && len(firstNonEmptyEncKey(store.uploads)) > 0
		store.mu.Unlock()
		if migrated {
			break
		}
		select {
		case <-deadline:
			t.Fatal("RunPeriodic did not migrate within deadline")
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunPeriodic did not return after ctx cancel")
	}
}

// TestMultipartBackfill_RunPeriodic_LogsErrors covers the branch in
// RunPeriodic where Run returns an error (here, the list call fails)
// and the periodic loop logs and waits for the next tick rather than
// exiting.
func TestMultipartBackfill_RunPeriodic_LogsErrors(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	store := newFakeMultipartStore()
	store.errList = errors.New("list always fails")
	mb := newBackfillForTest(store, noopLocker{}, enc, newFakeBackend(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		mb.RunPeriodic(ctx, 5*time.Millisecond)
		close(done)
	}()

	// Give the ticker enough time to fire at least once and hit the
	// error branch, then cancel.
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunPeriodic did not return after ctx cancel")
	}
	if store.listCallCount == 0 {
		t.Error("RunPeriodic never invoked Run")
	}
}

// firstNonEmptyEncKey returns the first non-zero-length encryption key
// across the upload map. Used by the RunPeriodic test to detect the
// post-migration state without depending on iteration order.
func firstNonEmptyEncKey(m map[string]core.MultipartUpload) []byte {
	for _, mu := range m { //nolint:gocritic // map value copy unavoidable in test fake
		if len(mu.EncryptionKey) > 0 {
			return mu.EncryptionKey
		}
	}
	return nil
}
