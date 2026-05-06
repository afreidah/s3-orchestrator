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
	"github.com/afreidah/s3-orchestrator/internal/proxy"
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
	mock := &testutil.MockStore{}
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
	mock := &testutil.MockStore{
		GetAllLocationsResp: []core.ObjectLocation{{ObjectKey: "foo", BackendName: "b1"}},
	}
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	var lv slog.LevelVar
	h := &Handler{dbCB: cb, objects: mock, cleanup: mock, token: "test-token", logLevel: &lv}
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
	h := newBackfillHandlerFixture(t, &testutil.MockStore{})
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
	mock := &testutil.MockStore{LegacyMultipartErr: errors.New("legacy list query failed")}
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
