// -------------------------------------------------------------------------------
// Admin Exported-Ops Tests
//
// Author: Alex Freidah
//
// Covers the skipped-path branches of the four exported operations the UI
// delegates to: Replicate, Scrub, BackfillChecksums, and EncryptExisting.
// Each operation has a documented "do nothing when not configured"
// behaviour that the UI relies on to render a status banner instead of an
// error. The shared newTestHandlerWithManager fixture leaves replication
// factor at 1, the encryptor nil, and integrity disabled  -  exactly the
// conditions every skipped path checks.
// -------------------------------------------------------------------------------

package admin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
)

// fakeBackend is a minimal in-memory ObjectBackend used by the bulk-rewrite
// happy-path tests. GetObject returns a fixed payload and PutObject is a
// no-op recorder so the rewrite closure can run end-to-end without real
// network I/O.
type fakeBackend struct {
	payload []byte
	puts    atomic.Int64
}

// GetObject returns object.
func (f *fakeBackend) GetObject(_ context.Context, _, _ string) (*backend.GetObjectResult, error) {
	body := io.NopCloser(bytes.NewReader(f.payload))
	return &backend.GetObjectResult{
		Body:        body,
		Size:        int64(len(f.payload)),
		ContentType: "application/octet-stream",
	}, nil
}

// PutObject satisfies backend.ObjectBackend by draining the supplied
// body (so the upload-side reader hits EOF) and recording the put
// count for tests that assert how many uploads the bulk-rewrite loop
// performed.
func (f *fakeBackend) PutObject(_ context.Context, _ string, body io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
	if _, err := io.Copy(io.Discard, body); err != nil {
		return "", err
	}
	f.puts.Add(1)
	return "etag", nil
}

// HeadObject satisfies backend.ObjectBackend by returning the fake
// payload's length. The HEAD path is exercised by the bulk-rewrite
// loop's pre-flight existence check before the real GET runs.
func (f *fakeBackend) HeadObject(_ context.Context, _ string) (*backend.HeadObjectResult, error) {
	return &backend.HeadObjectResult{Size: int64(len(f.payload))}, nil
}

// DeleteObject deletes object.
func (f *fakeBackend) DeleteObject(_ context.Context, _ string) error { return nil }

// rowEncAdmin returns a single UnencryptedLocation on the first call to
// ListUnencryptedLocations and an empty slice afterwards, so the
// bulk-rewrite loop processes exactly one object before terminating.
type rowEncAdmin struct {
	emptyEncAdmin
	row    core.UnencryptedLocation
	served atomic.Bool
	marked atomic.Bool
}

// ListUnencryptedLocations lists unencrypted locations.
func (r *rowEncAdmin) ListUnencryptedLocations(_ context.Context, _, _ int) ([]core.UnencryptedLocation, error) {
	if r.served.Swap(true) {
		return nil, nil
	}
	return []core.UnencryptedLocation{r.row}, nil
}

// MarkObjectEncrypted records that the bulk-rewrite loop reached the
// post-encrypt DB-update step so tests can assert the loop completed
// the encrypt + upload + mark-row sequence end-to-end.
func (r *rowEncAdmin) MarkObjectEncrypted(_ context.Context, _, _ string, _ []byte, _ string, _, _ int64) error {
	r.marked.Store(true)
	return nil
}

// emptyEncAdmin is a minimal EncryptionAdmin stub: every list returns no
// rows and every mutator is a no-op. Lets EncryptExisting's listFn closure
// run to completion against a non-nil encAdmin without inventing per-test
// fixture data.
type emptyEncAdmin struct{}

// ListEncryptedLocations lists encrypted locations.
func (emptyEncAdmin) ListEncryptedLocations(_ context.Context, _ string, _, _ int) ([]core.EncryptedLocation, error) {
	return nil, nil
}
// UpdateEncryptionKey updates encryption key.
func (emptyEncAdmin) UpdateEncryptionKey(_ context.Context, _, _ string, _ []byte, _ string) error {
	return nil
}
// ListUnencryptedLocations lists unencrypted locations.
func (emptyEncAdmin) ListUnencryptedLocations(_ context.Context, _, _ int) ([]core.UnencryptedLocation, error) {
	return nil, nil
}
// MarkObjectEncrypted is a no-op stub on emptyEncAdmin so the type
// satisfies the EncryptionAdmin interface. Tests that exercise the
// success path use rowEncAdmin instead.
func (emptyEncAdmin) MarkObjectEncrypted(_ context.Context, _, _ string, _ []byte, _ string, _, _ int64) error {
	return nil
}
// ListAllEncryptedLocations lists all encrypted locations.
func (emptyEncAdmin) ListAllEncryptedLocations(_ context.Context, _, _ int) ([]core.DecryptableLocation, error) {
	return nil, nil
}
// MarkObjectDecrypted is a no-op stub on emptyEncAdmin so the type
// satisfies the EncryptionAdmin interface for the decrypt-existing
// path. Tests that exercise the success path supply a different
// fixture.
func (emptyEncAdmin) MarkObjectDecrypted(_ context.Context, _, _ string, _ int64) error {
	return nil
}

// enableIntegrityForTest stores an integrity-enabled config on the
// handler's BackendOps so the Scrub / BackfillChecksums skipped guard
// passes during a test. The fixture's BackendOps is a real
// *proxy.BackendManager, which exposes SetIntegrityConfig.
func enableIntegrityForTest(t *testing.T, h *Handler) {
	t.Helper()
	mgr, ok := h.backendOps.(*proxy.BackendManager)
	if !ok {
		t.Fatalf("backendOps is %T, want *proxy.BackendManager", h.backendOps)
	}
	mgr.SetIntegrityConfig(&config.IntegrityConfig{
		Enabled:           true,
		ScrubberBatchSize: 50,
	})
}

// TestReplicate_SkippedWhenFactorAtOne asserts that the exported
// Replicate method returns Status="skipped" and zero copies created when
// the replication factor is configured at 1 (no replicas to make). The
// fixture intentionally leaves Factor at 1.
func TestReplicate_SkippedWhenFactorAtOne(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)

	res, err := h.Replicate(context.Background())
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if res.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", res.Status)
	}
	if res.CopiesCreated != 0 {
		t.Errorf("CopiesCreated = %d, want 0", res.CopiesCreated)
	}
	if res.Reason == "" {
		t.Error("Reason is empty; want non-empty explanation")
	}
}

// TestScrub_SkippedWhenIntegrityDisabled asserts that Scrub returns
// Status="skipped" when the integrity config has not been set on the
// manager (the default in the test fixture).
func TestScrub_SkippedWhenIntegrityDisabled(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)

	res := h.Scrub(context.Background(), 0)
	if res.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", res.Status)
	}
	if res.Checked != 0 || res.Failed != 0 {
		t.Errorf("Checked=%d Failed=%d, want both 0", res.Checked, res.Failed)
	}
	if res.Reason == "" {
		t.Error("Reason is empty; want non-empty explanation")
	}
}

// TestBackfillChecksums_SkippedWhenIntegrityDisabled asserts that
// BackfillChecksums skips and reports a reason when integrity is off.
func TestBackfillChecksums_SkippedWhenIntegrityDisabled(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)

	res := h.BackfillChecksums(context.Background(), 0)
	if res.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", res.Status)
	}
	if res.Processed != 0 {
		t.Errorf("Processed = %d, want 0", res.Processed)
	}
	if res.Reason == "" {
		t.Error("Reason is empty; want non-empty explanation")
	}
}

// TestEncryptExisting_SkippedWhenEncryptorNil asserts that
// EncryptExisting reports skipped when the handler was built without an
// encryptor (the documented "encryption not enabled" branch the UI
// surfaces as a banner).
func TestEncryptExisting_SkippedWhenEncryptorNil(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)

	res := h.EncryptExisting(context.Background())
	if res.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", res.Status)
	}
	if res.Success != 0 || res.Failed != 0 || res.Total != 0 {
		t.Errorf("counts = (success=%d failed=%d total=%d), want all 0",
			res.Success, res.Failed, res.Total)
	}
	if res.Reason == "" {
		t.Error("Reason is empty; want non-empty explanation")
	}
}

// TestEncryptExisting_HappyPathEmptyStore asserts that with both an
// encryptor and an admin store wired in, EncryptExisting walks the
// pagination loop, sees no rows on the first batch, and returns
// Status="complete" with zero counts. Drives the listFn closure that the
// nil-encryptor test cannot reach.
func TestEncryptExisting_HappyPathEmptyStore(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)

	// 32-byte base64-encoded master key for the local config-key provider.
	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-key")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	h.encryptor = enc
	h.encAdmin = emptyEncAdmin{}

	res := h.EncryptExisting(context.Background())
	if res.Status != "complete" {
		t.Errorf("Status = %q, want complete", res.Status)
	}
	if res.Total != 0 || res.Success != 0 || res.Failed != 0 {
		t.Errorf("counts = (success=%d failed=%d total=%d), want all 0",
			res.Success, res.Failed, res.Total)
	}
}

// TestEncryptExisting_HappyPathOneRow asserts the full bulk-rewrite path
// runs end-to-end on a single row backed by a fake backend: the listFn
// closure yields the row, processBulkLocation downloads from the fake,
// the rewrite closure encrypts, the fake's PutObject accepts the
// ciphertext, and the dbUpdate closure marks the object encrypted.
func TestEncryptExisting_HappyPathOneRow(t *testing.T) {
	t.Parallel()
	mock := testutil.NewMockStore(t)
	fake := &fakeBackend{payload: []byte("hello world")}
	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"backend-a": fake},
		Stores:          mock,
		Dashboard:       mock,
		Metrics:         mock,
		Order:           []string{"backend-a"},
		RoutingStrategy: config.RoutingPack,
	})
	proxytest.AttachWorkers(mgr, mock)

	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-key")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	encAdmin := &rowEncAdmin{row: core.UnencryptedLocation{
		ObjectKey:   "bucket/file.txt",
		BackendName: "backend-a",
		SizeBytes:   int64(len(fake.payload)),
	}}

	h := &Handler{
		log:        slog.Default().With(logfmt.Component("admin")),
		backendOps: mgr,
		replicator: mgr.Replicator,
		overRep:    mgr.OverReplicationCleaner,
		drain:      mgr.DrainManager,
		scrubber:   mgr.Scrubber,
		objects:    mock,
		cleanup:    mock,
		encAdmin:   encAdmin,
		encryptor:  enc,
		token:      "test-token",
	}

	res := h.EncryptExisting(context.Background())
	if res.Status != "complete" {
		t.Errorf("Status = %q, want complete", res.Status)
	}
	if res.Total != 1 {
		t.Errorf("Total = %d, want 1", res.Total)
	}
	if res.Success != 1 || res.Failed != 0 {
		t.Errorf("counts = (success=%d failed=%d), want (1, 0)", res.Success, res.Failed)
	}
	if !encAdmin.marked.Load() {
		t.Error("MarkObjectEncrypted was not called; dbUpdate closure did not run")
	}
	if fake.puts.Load() != 1 {
		t.Errorf("fakeBackend.PutObject calls = %d, want 1", fake.puts.Load())
	}
}

// TestReplicate_HappyPathEmptyStore asserts that with replication factor
// > 1 and no under-replicated objects in the store, Replicate returns
// Status="ok" with zero copies created. Exercises the post-skipped path
// the factor=1 test cannot reach.
func TestReplicate_HappyPathEmptyStore(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	// Bump replication factor so the skipped guard passes.
	mgr := h.backendOps.(*proxy.BackendManager)
	mgr.Replicator.SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 10})

	res, err := h.Replicate(context.Background())
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if res.Status != "ok" {
		t.Errorf("Status = %q, want ok", res.Status)
	}
	if res.CopiesCreated != 0 {
		t.Errorf("CopiesCreated = %d, want 0", res.CopiesCreated)
	}
}

// TestScrub_HappyPathEmptyStore asserts that with integrity enabled and
// no hashed objects in the store, Scrub returns Status="ok" with zero
// counts. Exercises the post-skipped pagination path that the
// nil-encryption test cannot reach.
func TestScrub_HappyPathEmptyStore(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	// Enable integrity on the embedded manager so Scrub does not skip.
	enableIntegrityForTest(t, h)

	res := h.Scrub(context.Background(), 0)
	if res.Status != "ok" {
		t.Errorf("Status = %q, want ok", res.Status)
	}
	if res.Checked != 0 || res.Failed != 0 {
		t.Errorf("Checked=%d Failed=%d, want both 0", res.Checked, res.Failed)
	}
}

// TestBackfillChecksums_HappyPathEmptyStore asserts that with integrity
// enabled and no objects missing a hash, BackfillChecksums returns
// Status="ok" Processed=0 immediately.
func TestBackfillChecksums_HappyPathEmptyStore(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	enableIntegrityForTest(t, h)

	res := h.BackfillChecksums(context.Background(), 0)
	if res.Status != "ok" {
		t.Errorf("Status = %q, want ok", res.Status)
	}
	if res.Processed != 0 {
		t.Errorf("Processed = %d, want 0", res.Processed)
	}
}

// TestNew_FromDeps asserts that the exported New constructor wires every
// dep onto the resulting Handler. Goes through the public surface so the
// 0%-coverage entry on New no longer hides bugs in the wiring.
func TestNew_FromDeps(t *testing.T) {
	t.Parallel()
	// Reuse the fixture's plumbing to build a minimal Deps bag. The
	// fixture builds a Handler directly; here we go through New so the
	// constructor is exercised in coverage too.
	src := newTestHandlerWithManager(t)
	deps := &Deps{
		BackendOps: src.backendOps,
		Replicator: src.replicator,
		OverRep:    src.overRep,
		Drain:      src.drain,
		Scrubber:   src.scrubber,
		Lifecycle:  src.lifecycle,
		DBCB:       src.dbCB,
		Objects:    src.objects,
		Cleanup:    src.cleanup,
		Token:      src.token,
		LogLevel:   src.logLevel,
	}

	h := New(deps)
	if h == nil {
		t.Fatal("New returned nil")
	}
	if h.token != src.token {
		t.Errorf("token = %q, want %q", h.token, src.token)
	}
	if h.replicator != src.replicator {
		t.Error("replicator not threaded through")
	}
	if h.scrubber != src.scrubber {
		t.Error("scrubber not threaded through")
	}
}
