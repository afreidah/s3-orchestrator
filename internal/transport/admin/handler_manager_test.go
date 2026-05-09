// -------------------------------------------------------------------------------
// Admin Handler Tests with a Real BackendManager
//
// Author: Alex Freidah
//
// Extends handler_test.go, which only exercises the auth and input-validation
// paths, with tests that route through a real BackendManager backed by the
// shared testutil.MockStore. Covers status, cleanup queue, replication,
// drain, and integrity-skip branches of the admin API.
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
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// newTestHandlerWithManager returns a Handler backed by a real BackendManager
// wrapping testutil.MockStore. Suitable for exercising handlers that reach
// into manager or cb-store methods. Encryptor, rawStore, and reconciler are
// nil  -  handlers that require them should assert the documented nil-handling
// behaviour rather than the happy path.
func newTestHandlerWithManager(t *testing.T) *Handler {
	t.Helper()
	mock := testutil.NewMockStore(t)
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
	})
	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{},
		Stores:          proxytest.StoresFromMock(mock),
		Dashboard:       mock,
		Metrics:         mock,
		Order:           []string{},
		RoutingStrategy: config.RoutingPack,
	})
	proxytest.AttachWorkers(mgr, mock)
	// Empty reloadable configs so Replicator.Config()/Scrubber.Config() return
	// sentinel states the handlers can interpret.
	mgr.Replicator.SetConfig(&config.ReplicationConfig{Factor: 1})
	mgr.OverReplicationCleaner.SetConfig(&config.ReplicationConfig{Factor: 1})

	var lv slog.LevelVar
	lv.Set(slog.LevelInfo)
	return &Handler{
		log:        slog.Default().With(logfmt.Component("admin")),
		backendOps: mgr,
		replicator: mgr.Replicator,
		overRep:    mgr.OverReplicationCleaner,
		drain:      mgr.DrainManager,
		scrubber:   mgr.Scrubber,
		lifecycle:  mock,
		dbCB:       cb,
		objects:    mock,
		cleanup:    mock,
		token:      "test-token",
		logLevel:   &lv,
	}
}

// doAuth builds a request pre-populated with the correct admin token.
func doAuth(method, path string, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Admin-Token", "test-token")
	return req
}

// TestHandleStatus_EmptyBackends covers the status path with a well-formed
// manager and no backends configured. Should return 200 with empty arrays
// rather than 500.
func TestHandleStatus_EmptyBackends(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/status", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp["db_healthy"]; !ok {
		t.Error("response missing db_healthy")
	}
	if _, ok := resp["backends"]; !ok {
		t.Error("response missing backends")
	}
}

// TestHandleCleanupQueue_ReturnsDepth covers the cleanup-queue GET with an
// empty mock store.
func TestHandleCleanupQueue_ReturnsDepth(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/cleanup-queue", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleObjectLocations_Happy covers the key-resolution path. The mock
// is pre-seeded so GetAllObjectLocations returns a non-empty slice.
func TestHandleObjectLocations_Happy(t *testing.T) {
	t.Parallel()
	mock := testutil.NewMockStore(t)
	mock.GetAllLocationsResp = []core.ObjectLocation{{ObjectKey: "foo", BackendName: "b1"}}
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	var lv slog.LevelVar
	h := &Handler{log: slog.Default().With(logfmt.Component("admin")), dbCB: cb, objects: mock, cleanup: mock, token: "test-token", logLevel: &lv}
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/object-locations?key=foo", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleObjectLocations_NotFound covers the 500 path when the store
// returns ErrObjectNotFound (the default MockStore behaviour). Handler
// currently does not distinguish not-found from other store errors.
func TestHandleObjectLocations_NotFound(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/object-locations?key=ghost", ""))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleUsageFlush_Success exercises the usage-flush POST path.
func TestHandleUsageFlush_Success(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/usage-flush", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleReplicate_NoReplicationConfigured hits replicate when the
// reloadable replication config is at factor=1; worker should short-circuit.
func TestHandleReplicate_NoReplicationConfigured(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/replicate", ""))

	// Either 200 (no-op) or a well-formed error; never a crash or 500 from
	// a nil-deref.
	if w.Code >= 500 {
		t.Fatalf("status = %d, want <500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleOverReplicationStatus_EmptyBackends covers over-replication
// status with no backends.
func TestHandleOverReplicationStatus_EmptyBackends(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/over-replication", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleOverReplicationClean_EmptyBackends covers over-replication
// cleanup with no backends.
func TestHandleOverReplicationClean_EmptyBackends(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/over-replication", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleScrub_IntegrityDisabled: with the default (zero-value) integrity
// config the scrubber is disabled and handleScrub returns a 200 with
// status=skipped, covering the early-exit branch.
func TestHandleScrub_IntegrityDisabled(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/scrub", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "skipped" {
		t.Errorf("status = %v, want %q", resp["status"], "skipped")
	}
}

// TestHandleBackfillChecksums_IntegrityDisabled mirrors scrub: the
// integrity-disabled short-circuit returns status=skipped.
func TestHandleBackfillChecksums_IntegrityDisabled(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/backfill-checksums", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleReconcile_NilReconciler covers the 503 path taken when no
// reconciler is wired up (the common single-instance default).
func TestHandleReconcile_NilReconciler(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/reconcile", ""))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleMultipartDEKBackfill_NotConfigured covers the 503 path taken
// when proxy-side encryption is disabled. The backfill worker is only
// registered with DI when encryption is on, so a request against an
// unencrypted deployment must surface a clean 503.
func TestHandleMultipartDEKBackfill_NotConfigured(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/multipart-dek-backfill", ""))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// newBackfillHandlerFixture wires a Handler with a real worker.MultipartBackfill
// backed by the supplied MockStore. The store stays caller-controlled so each
// test can flip a single error knob to drive a specific branch.
func newBackfillHandlerFixture(t *testing.T, mock *testutil.MockStore) *Handler {
	t.Helper()
	h := newTestHandlerWithManager(t)
	enc, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	encryptor, err := encryption.NewEncryptor(enc, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	store := struct {
		core.MultipartStore
		core.AdvisoryLocker
	}{MultipartStore: mock, AdvisoryLocker: mock}
	h.multipartBackfill = worker.NewMultipartBackfill(store, encryptor, func(string) (backend.ObjectBackend, error) {
		return nil, errors.New("no backend needed")
	}, worker.MultipartBackfillConfig{})
	return h
}

// TestHandleMultipartDEKBackfill_OK covers the success path. A worker
// wired against an empty MockStore reports zero migrations and the
// handler must return 200 with {"status":"ok","migrated":0}.
func TestHandleMultipartDEKBackfill_OK(t *testing.T) {
	t.Parallel()
	h := newBackfillHandlerFixture(t, testutil.NewMockStore(t))
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/multipart-dek-backfill", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
}

// TestHandleMultipartDEKBackfill_RunFails covers the 500 path: the
// worker's RunOnce surfaces an error (here, the legacy-list query
// fails). The handler must propagate it as a 500 with the migrated
// count from before the failure.
func TestHandleMultipartDEKBackfill_RunFails(t *testing.T) {
	t.Parallel()
	mock := testutil.NewMockStore(t)
	mock.LegacyMultipartErr = errors.New("legacy list query failed")
	h := newBackfillHandlerFixture(t, mock)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/multipart-dek-backfill", ""))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] == nil {
		t.Errorf("response missing error field: %v", resp)
	}
}

// TestHandleStartDrain_UnknownBackend drains a backend that doesn't exist;
// DrainManager should return an error and handleStartDrain translates to 400.
func TestHandleStartDrain_UnknownBackend(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/backends/nope/drain", ""))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDrainProgress_UnknownBackend asks for progress on a backend
// that was never drained. DrainManager.GetDrainProgress may return 404 or
// a well-formed "not draining" response; accept either as long as it's not
// a 5xx crash.
func TestHandleDrainProgress_UnknownBackend(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/backends/nope/drain", ""))

	if w.Code >= 500 {
		t.Fatalf("status = %d, want <500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleCancelDrain_NoActiveDrain cancels a drain that was never started;
// handler returns 400 per its contract.
func TestHandleCancelDrain_NoActiveDrain(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/backends/nope/drain", ""))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleRemoveBackend_NonPurge covers the non-destructive DELETE path:
// without ?purge=true the handler removes DB records immediately and
// returns 200 "backend removed".
func TestHandleRemoveBackend_NonPurge(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/backends/someb", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleRemoveBackend_PurgePhase1 covers the two-phase purge protocol:
// without a confirm token, handler previews the destruction and returns a
// confirmation token (status=confirmation required).
func TestHandleRemoveBackend_PurgePhase1(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/backends/b1?purge=true", ""))

	// MockStore is permissive  -  it may return 200 with a confirm_token, or
	// 400 if the backend doesn't exist in its view. Either is a contract-
	// compliant response; a 5xx is not.
	if w.Code >= 500 {
		t.Fatalf("status = %d, want <500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleRotateEncryptionKey_NoEncryptor covers the nil-encryptor branch
// (encryption disabled).
func TestHandleRotateEncryptionKey_NoEncryptor(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/rotate-encryption-key", ""))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleCleanupQueue_DepthError covers the error branch where
// CleanupQueueDepth fails. The handler logs and surfaces a 500;
// asserting the status code is enough to drive the log + return.
func TestHandleCleanupQueue_DepthError(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	// Swap the cleanup store for one whose depth call fails. The
	// handler reads h.cleanup directly, so assigning a fresh mock
	// is sufficient.
	cleanupMock := testutil.NewMockStore(t)
	cleanupMock.CleanupQueueDepthErr = errors.New("db down")
	h.cleanup = cleanupMock
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/cleanup-queue", ""))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleCleanupQueue_PendingError covers the second error branch
// where GetPendingCleanups fails after CleanupQueueDepth succeeds.
func TestHandleCleanupQueue_PendingError(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	cleanupMock := testutil.NewMockStore(t)
	cleanupMock.CleanupQueueDepthResp = 5
	cleanupMock.PendingCleanupsErr = errors.New("query failed")
	h.cleanup = cleanupMock
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/cleanup-queue", ""))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleUsageFlush_Error covers the FlushUsage error branch in
// handleUsageFlush. The fixture's BackendManager wraps a MockStore
// whose FlushUsageDeltas call honours FlushUsageErr.
func TestHandleUsageFlush_Error(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	// h.backendOps is a *proxy.BackendManager backed by a MockStore.
	// We cannot easily inject an error through it, so swap the
	// BackendOps interface with a stub that fails FlushUsage.
	h.backendOps = &flushUsageFailingOps{inner: h.backendOps}
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/usage-flush", ""))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// flushUsageFailingOps wraps an inner BackendOps but always errors
// on FlushUsage, letting the test cover handleUsageFlush's error
// branch without modifying the underlying mock store.
type flushUsageFailingOps struct {
	inner BackendOps
}

func (f *flushUsageFailingOps) GetDashboardData(ctx context.Context) (*dashboard.Data, error) {
	return f.inner.GetDashboardData(ctx)
}
func (f *flushUsageFailingOps) FlushUsage(_ context.Context) error {
	return errors.New("flush failed")
}
func (f *flushUsageFailingOps) UpdateQuotaMetrics(ctx context.Context) error {
	return f.inner.UpdateQuotaMetrics(ctx)
}
func (f *flushUsageFailingOps) RecordUsage(name string, req, in, out int64) {
	f.inner.RecordUsage(name, req, in, out)
}
func (f *flushUsageFailingOps) GetBackend(name string) (backend.ObjectBackend, error) {
	return f.inner.GetBackend(name)
}
func (f *flushUsageFailingOps) IntegrityConfig() *config.IntegrityConfig {
	return f.inner.IntegrityConfig()
}

// TestHandleReconcile_UsesContext is a smoke test that the handler threads
// the request context correctly (reconciler is nil, so we get 503, but the
// path should not panic regardless of context state).
// TestHandleReconcile_CancelledContext verifies handle reconcile_cancelled context.
// TestHandleReconcile_CancelledContext verifies handle reconcile_cancelled context.
func TestHandleReconcile_CancelledContext(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := doAuth(http.MethodPost, "/admin/api/reconcile", "")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// allFailingOps wraps the real BackendOps but errors on every method
// that has an error return. Lets a single test exercise every
// admin handler's "downstream call failed" branch without writing
// per-handler error fixtures.
type allFailingOps struct{}

func (allFailingOps) GetDashboardData(_ context.Context) (*dashboard.Data, error) {
	return nil, errors.New("dashboard down")
}
func (allFailingOps) FlushUsage(_ context.Context) error {
	return errors.New("flush down")
}
func (allFailingOps) UpdateQuotaMetrics(_ context.Context) error {
	return errors.New("quota down")
}
func (allFailingOps) RecordUsage(_ string, _, _, _ int64) {}
func (allFailingOps) GetBackend(_ string) (backend.ObjectBackend, error) {
	return nil, errors.New("backend not found")
}
func (allFailingOps) IntegrityConfig() *config.IntegrityConfig {
	return nil
}

// TestHandleStatus_DashboardError covers the error branch where the
// backend ops dashboard call fails.
func TestHandleStatus_DashboardError(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	h.backendOps = allFailingOps{}
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/status", ""))

	if w.Code < 500 {
		t.Fatalf("status = %d, want 5xx; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleObjectLocations_StoreError covers the error branch where
// the object store fails to fetch object locations.
func TestHandleObjectLocations_StoreError(t *testing.T) {
	t.Parallel()
	mock := testutil.NewMockStore(t)
	mock.GetAllLocationsErr = errors.New("query failed")
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	var lv slog.LevelVar
	h := &Handler{log: slog.Default().With(logfmt.Component("admin")), dbCB: cb, objects: mock, cleanup: mock, token: "test-token", logLevel: &lv}
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/object-locations?key=foo", ""))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleLogLevel_Get covers the GET branch of handleLogLevel.
func TestHandleLogLevel_Get(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/log-level", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleLogLevel_PutValid covers the happy-path PUT branch.
func TestHandleLogLevel_PutValid(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPut, "/admin/api/log-level", `{"level":"debug"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleLogLevel_PutInvalidBody covers the JSON-decode error
// branch in handleLogLevel.
func TestHandleLogLevel_PutInvalidBody(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPut, "/admin/api/log-level", `not json`))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}
