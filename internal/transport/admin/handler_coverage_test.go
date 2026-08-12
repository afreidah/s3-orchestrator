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

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// newCoverageHandler builds a Handler wired entirely from the generated ops
// mocks, so each test can dial in the precise branch it wants to exercise
// without standing up a BackendManager.
func newCoverageHandler(t *testing.T) *Handler {
	t.Helper()
	var lv slog.LevelVar
	lv.Set(slog.LevelInfo)
	return &Handler{
		log:        slog.Default().With(logfmt.Component("admin")),
		runtimeOps: newRuntimeOps(t),
		token:      "test-token",
		logLevel:   &lv,
		dbHealthy:  func() bool { return true },
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
	h := newCoverageHandler(t)
	h.dashboardOps = newDashboardOps(t, &dashboard.Data{
		BackendOrder: []string{"b1"},
		QuotaStats:   map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		ObjectCounts: map[string]int64{"b1": 5},
		UsageStats:   map[string]core.UsageStat{"b1": {APIRequests: 3, IngressBytes: 50, EgressBytes: 25}},
		UsagePeriod:  "2026-05",
	}, nil)

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
	h := newCoverageHandler(t)
	h.overRep = newOverRep(t, overRepStub{cfg: &config.ReplicationConfig{Factor: 2}, count: 7})

	w := httptest.NewRecorder()
	h.handleOverReplicationStatus(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/over-replication", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.OverReplicationStatusResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Pending != 7 || resp.Factor != 2 {
		t.Errorf("got factor=%d pending=%d, want 2/7", resp.Factor, resp.Pending)
	}
	// Status reports "ok" on the configured branch so the field means the same
	// thing here as on the endpoints that act.
	if resp.Status != "ok" || resp.Reason != "" {
		t.Errorf("got status=%q reason=%q, want ok with no reason", resp.Status, resp.Reason)
	}
}

// TestHandleOverReplicationStatus_Unconfigured pins the skipped branch: zeroed
// counts carrying the same status vocabulary as the replicate and clean
// endpoints rather than a sentence in the status field.
func TestHandleOverReplicationStatus_Unconfigured(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	h.overRep = newOverRep(t, overRepStub{cfg: &config.ReplicationConfig{Factor: 1}})

	w := httptest.NewRecorder()
	h.handleOverReplicationStatus(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/over-replication", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.OverReplicationStatusResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != "skipped" || resp.Reason == "" {
		t.Errorf("got status=%q reason=%q, want skipped with a reason", resp.Status, resp.Reason)
	}
	if resp.Factor != 0 || resp.Pending != 0 {
		t.Errorf("got factor=%d pending=%d, want both zero", resp.Factor, resp.Pending)
	}
}

// TestHandleOverReplicationStatus_CountError exercises the error branch.
func TestHandleOverReplicationStatus_CountError(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	h.overRep = newOverRep(t, overRepStub{cfg: &config.ReplicationConfig{Factor: 2}, countErr: errors.New("db down")})

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
	h := newCoverageHandler(t)
	h.backendOps = newBackendOps(t, backendOpsStub{})
	h.overRep = newOverRep(t, overRepStub{cfg: &config.ReplicationConfig{Factor: 2, BatchSize: 5}, cleaned: 3})

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
	h := newCoverageHandler(t)
	h.backendOps = newBackendOps(t, backendOpsStub{reconcileMap: map[string]int64{"e2": -163}})

	w := httptest.NewRecorder()
	h.handleReconcileUsage(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/usage-reconcile", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.UsageReconcileResponse
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
	h := newCoverageHandler(t)
	h.backendOps = newBackendOps(t, backendOpsStub{reconcileErr: errors.New("db down")})

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
	h := newCoverageHandler(t)
	h.backendOps = newBackendOps(t, backendOpsStub{})
	h.replicator = newReplicator(t, replicatorStub{cfg: &config.ReplicationConfig{Factor: 2}, created: 4})

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
	h := newCoverageHandler(t)
	h.backendOps = newBackendOps(t, backendOpsStub{integrity: &config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}})
	h.scrubber = newScrubber(t, &scrubberStub{scrubChecked: 12, scrubFailed: 1})

	w := httptest.NewRecorder()
	h.handleScrub(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/scrub?batch_size=10", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.ScrubResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Checked != 12 || resp.Failed != 1 {
		t.Errorf("got checked=%d failed=%d, want 12/1", resp.Checked, resp.Failed)
	}
	if resp.Status != "ok" || resp.Reason != "" {
		t.Errorf("got status=%q reason=%q, want ok with no reason", resp.Status, resp.Reason)
	}
}

// TestHandleBackfillChecksums_IntegrityEnabled drives the non-skip
// backfill path. The fake scrubber returns nextOffset=0 to terminate the
// paginated loop on the first batch.
func TestHandleBackfillChecksums_IntegrityEnabled(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	h.backendOps = newBackendOps(t, backendOpsStub{integrity: &config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}})
	h.scrubber = newScrubber(t, &scrubberStub{backfillProcessed: 8})

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
	h := newCoverageHandler(t)
	h.backendOps = newBackendOps(t, backendOpsStub{integrity: &config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}})
	scrubs := &scrubberStub{backfillProcessed: 10, backfillMore: true}
	sc := newScrubber(t, scrubs)
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
	if scrubs.backfillCalls != 3 {
		t.Errorf("backfillCalls = %d, want 3", scrubs.backfillCalls)
	}
}

// -------------------------------------------------------------------------
// RECONCILE
// -------------------------------------------------------------------------

// TestHandleReconcile_Success drives the happy reconcile path with a
// configured reconciler returning non-zero counts.
func TestHandleReconcile_Success(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	h.reconciler = newReconciler(t, &worker.ReconcileResult{Imported: 4, Removed: 1, BackendsScanned: 2}, nil)

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
	h := newCoverageHandler(t)
	h.reconciler = newReconciler(t, nil, errors.New("scan failed"))

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
	h := newCoverageHandler(t)
	h.SetReloadStatusProvider(func() *adminapi.ReloadStatusResponse { return nil })

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
	body2 := w.Body.Bytes()
	var resp adminapi.RotateEncryptionKeyResponse
	_ = json.Unmarshal(body2, &resp)
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
	if resp.Failed != 1 {
		t.Errorf("failed = %d, want 1 (malformed key trips UnpackKeyData)", resp.Failed)
	}

	// BulkEncryptionOutcome is embedded, so its fields must flatten into the
	// same top-level keys the endpoint has always emitted rather than nesting
	// under an object.
	var raw map[string]any
	_ = json.Unmarshal(body2, &raw)
	for _, k := range []string{"status", "rotated", "failed", "total"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("response is missing top-level %q: %s", k, body2)
		}
	}
	if len(raw) != 4 {
		t.Errorf("response has %d keys, want exactly 4: %s", len(raw), body2)
	}
}

// TestHandleScrub_ReportsUnreadableCount pins the count that used to be
// dropped. A pass that could not read half the copies must not report the same
// shape as a clean one, so the JSON response carries unreadable next to
// checked and failed.
func TestHandleScrub_ReportsUnreadableCount(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	h.backendOps = newBackendOps(t, backendOpsStub{integrity: &config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}})
	h.scrubber = newScrubber(t, &scrubberStub{scrubChecked: 4, scrubFailed: 1, scrubSkipped: 7})

	w := httptest.NewRecorder()
	h.handleScrub(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/scrub", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.ScrubResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Checked != 4 || resp.Failed != 1 || resp.Unreadable != 7 {
		t.Errorf("got checked=%d failed=%d unreadable=%d, want 4/1/7",
			resp.Checked, resp.Failed, resp.Unreadable)
	}
}

// TestHandleScrub_StreamSummaryReportsUnreadable drives the NDJSON path, where
// the terminal summary line is what an operator actually reads. Reporting only
// checked and failed there is what let a pass over unreadable copies look
// clean.
func TestHandleScrub_StreamSummaryReportsUnreadable(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	h.backendOps = newBackendOps(t, backendOpsStub{integrity: &config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}})
	h.scrubber = newScrubber(t, &scrubberStub{scrubChecked: 3, scrubFailed: 2, scrubSkipped: 5, scrubDeferred: 9})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/scrub", nil)
	req.Header.Set("Accept", adminstream.ContentType)
	w := httptest.NewRecorder()
	h.handleScrub(w, req)

	if ct := w.Header().Get("Content-Type"); ct != adminstream.ContentType {
		t.Fatalf("Content-Type = %q, want %q", ct, adminstream.ContentType)
	}

	var result adminstream.Event
	for line := range strings.SplitSeq(strings.TrimSpace(w.Body.String()), "\n") {
		var ev adminstream.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if ev.Kind == adminstream.KindResult {
			result = ev
		}
	}

	if result.Kind != adminstream.KindResult {
		t.Fatalf("no result event in stream: %s", w.Body.String())
	}
	if !strings.Contains(result.Message, "unreadable 5") {
		t.Errorf("summary = %q, want it to report unreadable 5", result.Message)
	}
	if got := result.Fields["unreadable"]; got != float64(5) {
		t.Errorf("fields[unreadable] = %v, want 5", got)
	}
	// Deferred copies were never selected, so a summary that omits them reports
	// a budget-limited sweep as a complete one.
	if !strings.Contains(result.Message, "deferred 9") {
		t.Errorf("summary = %q, want it to report deferred 9", result.Message)
	}
	if got := result.Fields["deferred"]; got != float64(9) {
		t.Errorf("fields[deferred] = %v, want 9", got)
	}
}
