// -------------------------------------------------------------------------------
// Admin Handler - Coverage-Focused Branch Tests
//
// Author: Alex Freidah
//
// Hand-rolled fakes for the narrow consumer interfaces (BackendOps,
// ReplicatorOps, OverReplicationOps, ScrubberOps, Reconciler) so the
// success branches of handlers that otherwise required a full
// BackendManager + worker fleet stay exercised. Pairs with the existing
// handler_manager_test.go which covers the empty/skip paths via a real
// manager.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// FAKES
// -------------------------------------------------------------------------

type fakeBackendOps struct {
	dashData     *dashboard.Data
	dashErr      error
	flushErr     error
	intCfg       *config.IntegrityConfig
	reconcileMap map[string]int64
	reconcileErr error
}

func (f *fakeBackendOps) GetDashboardData(_ context.Context) (*dashboard.Data, error) {
	return f.dashData, f.dashErr
}
func (f *fakeBackendOps) FlushUsage(_ context.Context) error         { return f.flushErr }
func (f *fakeBackendOps) UpdateQuotaMetrics(_ context.Context) error { return nil }
func (f *fakeBackendOps) ReconcileUsage(_ context.Context) (map[string]int64, error) {
	return f.reconcileMap, f.reconcileErr
}
func (f *fakeBackendOps) RecordUsage(_ string, _, _, _ int64) {}
func (f *fakeBackendOps) GetBackend(_ string) (backend.ObjectBackend, error) {
	return nil, errors.New("no backend")
}
func (f *fakeBackendOps) IntegrityConfig() *config.IntegrityConfig { return f.intCfg }

type fakeReplicator struct {
	cfg     *config.ReplicationConfig
	created int
	err     error
}

func (f *fakeReplicator) Config() *config.ReplicationConfig { return f.cfg }
func (f *fakeReplicator) Replicate(_ context.Context, _ config.ReplicationConfig, observer progress.Observer) (int, error) {
	for range f.created {
		progress.Track(observer, "fake-key", func() string { return progress.StatusOK })
	}
	return f.created, f.err
}

type fakeOverRep struct {
	cfg      *config.ReplicationConfig
	count    int64
	countErr error
	cleaned  int
	cleanErr error
}

func (f *fakeOverRep) Config() *config.ReplicationConfig { return f.cfg }
func (f *fakeOverRep) CountPending(_ context.Context, _ int) (int64, error) {
	return f.count, f.countErr
}
func (f *fakeOverRep) Clean(_ context.Context, _ config.ReplicationConfig, observer progress.Observer) (int, error) {
	for range f.cleaned {
		progress.Track(observer, "fake-key", func() string { return progress.StatusOK })
	}
	return f.cleaned, f.cleanErr
}

type fakeScrubber struct {
	scrubChecked, scrubFailed int
	backfillProcessed         int
	backfillMore              bool // when true, always report another batch (nextOffset != 0)
	backfillCalls             int
}

func (f *fakeScrubber) Scrub(_ context.Context, _ int, observer progress.Observer) (int, int) {
	for range f.scrubChecked {
		progress.Track(observer, "fake-key", func() string { return progress.StatusOK })
	}
	return f.scrubChecked, f.scrubFailed
}
func (f *fakeScrubber) Backfill(_ context.Context, batchSize, offset int, observer progress.Observer) (int, int) {
	f.backfillCalls++
	for range f.backfillProcessed {
		progress.Track(observer, "fake-key", func() string { return progress.StatusOK })
	}
	if f.backfillMore {
		return f.backfillProcessed, offset + batchSize
	}
	// Return processed for one batch then signal done with nextOffset=0.
	return f.backfillProcessed, 0
}

type fakeReconciler struct {
	result *worker.ReconcileResult
	err    error
}

func (f *fakeReconciler) Reconcile(_ context.Context, _ string) (*worker.ReconcileResult, error) {
	return f.result, f.err
}

func (f *fakeReconciler) ReconcileStreaming(_ context.Context, _ string, observer progress.Observer) (*worker.ReconcileResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	progress.Track(observer, "fake-backend", func() string { return progress.StatusOK })
	return f.result, f.err
}

// newCoverageHandler builds a Handler wired entirely from the lightweight
// fakes above so each test can dial in the precise branch it wants to
// exercise without standing up a BackendManager.
func newCoverageHandler() *Handler {
	var lv slog.LevelVar
	lv.Set(slog.LevelInfo)
	return &Handler{
		log:       slog.Default().With(logfmt.Component("admin")),
		token:     "test-token",
		logLevel:  &lv,
		dbHealthy: func() bool { return true },
	}
}

// -------------------------------------------------------------------------
// STATUS
// -------------------------------------------------------------------------

// TestHandleStatus_PopulatedDashboard drives the inner branches of
// handleStatus that the empty-backends test never enters: QuotaStats,
// ObjectCounts, and UsageStats lookups all succeeding for the same key.
func TestHandleStatus_PopulatedDashboard(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.backendOps = &fakeBackendOps{
		dashData: &dashboard.Data{
			BackendOrder: []string{"b1"},
			QuotaStats:   map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
			ObjectCounts: map[string]int64{"b1": 5},
			UsageStats:   map[string]core.UsageStat{"b1": {APIRequests: 3, IngressBytes: 50, EgressBytes: 25}},
			UsagePeriod:  "2026-05",
		},
	}

	w := httptest.NewRecorder()
	h.handleStatus(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/status", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	backends, ok := resp["backends"].([]any)
	if !ok || len(backends) != 1 {
		t.Fatalf("backends = %v, want length 1", resp["backends"])
	}
	row := backends[0].(map[string]any)
	if row["bytes_used"].(float64) != 100 || row["object_count"].(float64) != 5 || row["api_requests"].(float64) != 3 {
		t.Errorf("backend row not fully populated: %v", row)
	}
}

// -------------------------------------------------------------------------
// OVER-REPLICATION
// -------------------------------------------------------------------------

// TestHandleOverReplicationStatus_Configured exercises the factor > 1
// path that the existing skip-only test never reaches.
func TestHandleOverReplicationStatus_Configured(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.overRep = &fakeOverRep{cfg: &config.ReplicationConfig{Factor: 2}, count: 7}

	w := httptest.NewRecorder()
	h.handleOverReplicationStatus(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/over-replication", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["pending"].(float64) != 7 {
		t.Errorf("pending = %v, want 7", resp["pending"])
	}
}

// TestHandleOverReplicationStatus_CountError exercises the error branch.
func TestHandleOverReplicationStatus_CountError(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.overRep = &fakeOverRep{cfg: &config.ReplicationConfig{Factor: 2}, countErr: errors.New("db down")}

	w := httptest.NewRecorder()
	h.handleOverReplicationStatus(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/over-replication", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleOverReplicationClean_Configured exercises the success path
// including the batch_size query parameter parser.
func TestHandleOverReplicationClean_Configured(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.backendOps = &fakeBackendOps{}
	h.overRep = &fakeOverRep{cfg: &config.ReplicationConfig{Factor: 2, BatchSize: 5}, cleaned: 3}

	w := httptest.NewRecorder()
	h.handleOverReplicationClean(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/over-replication?batch_size=100", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["copies_removed"].(float64) != 3 {
		t.Errorf("copies_removed = %v, want 3", resp["copies_removed"])
	}
}

// -------------------------------------------------------------------------
// USAGE RECONCILE
// -------------------------------------------------------------------------

// TestHandleReconcileUsage_Success exercises the success path: the handler
// returns the per-backend bytes_used corrections from the store.
func TestHandleReconcileUsage_Success(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.backendOps = &fakeBackendOps{reconcileMap: map[string]int64{"e2": -163}}

	w := httptest.NewRecorder()
	h.handleReconcileUsage(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/usage-reconcile", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Status      string           `json:"status"`
		Adjustments map[string]int64 `json:"adjustments"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != "reconciled" {
		t.Errorf("status = %q, want reconciled", resp.Status)
	}
	if resp.Adjustments["e2"] != -163 {
		t.Errorf("e2 adjustment = %d, want -163", resp.Adjustments["e2"])
	}
}

// TestHandleReconcileUsage_Error exercises the failure path: a store error
// surfaces as a 500.
func TestHandleReconcileUsage_Error(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.backendOps = &fakeBackendOps{reconcileErr: errors.New("db down")}

	w := httptest.NewRecorder()
	h.handleReconcileUsage(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/usage-reconcile", nil))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// -------------------------------------------------------------------------
// REPLICATE
// -------------------------------------------------------------------------

// TestHandleReplicate_Configured exercises the non-skip path through
// Replicate so the success-branch JSON envelope and UpdateQuotaMetrics
// hook stay covered.
func TestHandleReplicate_Configured(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.backendOps = &fakeBackendOps{}
	h.replicator = &fakeReplicator{cfg: &config.ReplicationConfig{Factor: 2}, created: 4}

	w := httptest.NewRecorder()
	h.handleReplicate(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/replicate", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["copies_created"].(float64) != 4 {
		t.Errorf("copies_created = %v, want 4", resp["copies_created"])
	}
}

// -------------------------------------------------------------------------
// INTEGRITY
// -------------------------------------------------------------------------

// TestHandleScrub_IntegrityEnabled exercises the non-skip Scrub path so
// the success-branch handler and the typed Scrub method stay covered.
func TestHandleScrub_IntegrityEnabled(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.backendOps = &fakeBackendOps{intCfg: &config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}}
	h.scrubber = &fakeScrubber{scrubChecked: 12, scrubFailed: 1}

	w := httptest.NewRecorder()
	h.handleScrub(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/scrub?batch_size=10", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["checked"].(float64) != 12 || resp["failed"].(float64) != 1 {
		t.Errorf("counts wrong: %v", resp)
	}
}

// TestHandleBackfillChecksums_IntegrityEnabled drives the non-skip
// backfill path. The fake scrubber returns nextOffset=0 to terminate the
// paginated loop on the first batch.
func TestHandleBackfillChecksums_IntegrityEnabled(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.backendOps = &fakeBackendOps{intCfg: &config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}}
	h.scrubber = &fakeScrubber{backfillProcessed: 8}

	w := httptest.NewRecorder()
	h.handleBackfillChecksums(w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/backfill-checksums", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["processed"].(float64) != 8 {
		t.Errorf("processed = %v, want 8", resp["processed"])
	}
	if resp["done"] != true {
		t.Errorf("done = %v, want true", resp["done"])
	}
}

// TestHandleBackfillChecksums_BoundedByMax verifies that ?max caps the
// objects processed in one request (so a single call fits the client
// timeout) and that the response reports done=false when more remain.
// delay_ms exercises the inter-batch pacing path.
func TestHandleBackfillChecksums_BoundedByMax(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.backendOps = &fakeBackendOps{intCfg: &config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}}
	sc := &fakeScrubber{backfillProcessed: 10, backfillMore: true}
	h.scrubber = sc

	w := httptest.NewRecorder()
	h.handleBackfillChecksums(w, httptest.NewRequestWithContext(
		context.Background(), http.MethodPost, "/admin/api/backfill-checksums?max=25&delay_ms=1", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	// 10 processed per batch, stops once total >= 25: 3 batches => 30.
	if resp["processed"].(float64) != 30 {
		t.Errorf("processed = %v, want 30", resp["processed"])
	}
	if resp["done"] != false {
		t.Errorf("done = %v, want false (backlog not drained)", resp["done"])
	}
	if sc.backfillCalls != 3 {
		t.Errorf("backfillCalls = %d, want 3", sc.backfillCalls)
	}
}

// -------------------------------------------------------------------------
// RECONCILE
// -------------------------------------------------------------------------

// TestHandleReconcile_Success drives the happy reconcile path with a
// configured reconciler returning non-zero counts.
func TestHandleReconcile_Success(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.reconciler = &fakeReconciler{result: &worker.ReconcileResult{Imported: 4, Removed: 1, BackendsScanned: 2}}

	w := httptest.NewRecorder()
	h.handleReconcile(w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/reconcile?backend=b1", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["imported"].(float64) != 4 || resp["removed"].(float64) != 1 {
		t.Errorf("counts wrong: %v", resp)
	}
}

// TestHandleReconcile_Error pins the error branch when the reconciler
// returns a non-nil error.
func TestHandleReconcile_Error(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.reconciler = &fakeReconciler{err: errors.New("scan failed")}

	w := httptest.NewRecorder()
	h.handleReconcile(w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/reconcile", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// -------------------------------------------------------------------------
// BULK REWRITE ROW ADAPTERS
// -------------------------------------------------------------------------

// TestBulkRewriteAdapters exercises the encryptRow / decryptRow
// adapter methods so the trivial getters are not reported as 0%.
func TestBulkRewriteAdapters(t *testing.T) {
	t.Parallel()

	er := &encryptRow{UnencryptedLocation: core.UnencryptedLocation{ObjectKey: "k", BackendName: "b", SizeBytes: 42}}
	if er.rewriteKey() != "k" || er.rewriteBackend() != "b" || er.rewriteSize() != 42 {
		t.Errorf("encryptRow accessors wrong: %+v", er)
	}

	dr := &decryptRow{DecryptableLocation: core.DecryptableLocation{ObjectKey: "x", BackendName: "y", SizeBytes: 7}}
	if dr.rewriteKey() != "x" || dr.rewriteBackend() != "y" || dr.rewriteSize() != 7 {
		t.Errorf("decryptRow accessors wrong: %+v", dr)
	}
}

// -------------------------------------------------------------------------
// RELOAD STATUS
// -------------------------------------------------------------------------

// TestHandleReloadStatus_ProviderReturnsNil exercises the branch where
// the provider is wired but the runtime has not yet captured a reload
// result. The existing tests cover only the nil-provider and result-
// returned branches; this fills the middle case.
func TestHandleReloadStatus_ProviderReturnsNil(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler()
	h.SetReloadStatusProvider(func() any { return nil })

	w := httptest.NewRecorder()
	h.handleReloadStatus(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/reload-status", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "no_reload_yet") {
		t.Errorf("body = %q, want no_reload_yet placeholder", got)
	}
}

// -------------------------------------------------------------------------
// CACHE
// -------------------------------------------------------------------------

// TestHandleCacheInvalidateKey_EmptyKey exercises the empty-key branch
// of handleCacheInvalidateKey (400 with explicit error message). The
// route registration sets `{key...}` so reaching this branch through
// the mux requires a direct call.
func TestHandleCacheInvalidateKey_EmptyKey(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithCache(t)
	w := httptest.NewRecorder()
	// No PathValue set on the bare request -> key resolves to "".
	h.handleCacheInvalidateKey(w, httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/admin/api/cache/keys/", nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// -------------------------------------------------------------------------
// KEY ROTATION
// -------------------------------------------------------------------------

// singleRowEncAdmin is an EncryptionAdmin stub that returns a single
// EncryptedLocation on the first ListEncryptedLocations call and an
// empty slice afterwards. Lets the rotation loop exercise rotateBatch
// + rotateOneLocation through the rotateOne unpack failure branch
// (the EncryptionKey is intentionally malformed).
type singleRowEncAdmin struct {
	emptyEncAdmin
	sent bool
}

func (r *singleRowEncAdmin) ListEncryptedLocations(_ context.Context, _ string, _, _ int) ([]core.EncryptedLocation, error) {
	if r.sent {
		return nil, nil
	}
	r.sent = true
	return []core.EncryptedLocation{
		{ObjectKey: "k1", BackendName: "b1", EncryptionKey: []byte{0x01}, KeyID: "old"},
	}, nil
}

// TestHandleDecryptExisting_HappyEmpty wires an encryptor + a stub
// admin so handleDecryptExisting walks the bulk-rewrite loop, sees an
// empty list on the first batch, and returns "complete" with zero
// counts. Drives the lines around runBulkRewriteCounts that the
// nil-encryptor test cannot reach.
func TestHandleDecryptExisting_HappyEmpty(t *testing.T) {
	t.Parallel()
	h := newRotateEncryptionKeyHandler(t) // gives us encryptor + emptyEncAdmin

	w := httptest.NewRecorder()
	h.handleDecryptExisting(w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/decrypt-existing", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"].(string) != "complete" {
		t.Errorf("status = %q, want complete", resp["status"])
	}
	if resp["total"].(float64) != 0 {
		t.Errorf("total = %v, want 0", resp["total"])
	}
}

// TestHandleRotateEncryptionKey_DrivesListLoop wires an encryptor +
// a stub admin that returns one malformed EncryptedLocation so the
// rotation pipeline runs end-to-end: list -> rotateBatch ->
// rotateOneLocation. The intentionally-malformed key trips the
// UnpackKeyData branch so the success counter remains 0 and the
// failed counter increments  -  the goal here is coverage of the
// loop body, not a particular outcome.
func TestHandleRotateEncryptionKey_DrivesListLoop(t *testing.T) {
	t.Parallel()
	h := newRotateEncryptionKeyHandler(t)
	h.encAdmin = &singleRowEncAdmin{}

	body := `{"old_key_id":"old"}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/rotate-encryption-key", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.handleRotateEncryptionKey(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["total"].(float64) != 1 {
		t.Errorf("total = %v, want 1", resp["total"])
	}
	if resp["failed"].(float64) != 1 {
		t.Errorf("failed = %v, want 1 (malformed key trips UnpackKeyData)", resp["failed"])
	}
}
