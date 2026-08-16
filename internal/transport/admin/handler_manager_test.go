// -------------------------------------------------------------------------------
// Admin Handler Tests with a Real BackendManager
//
// Author: Alex Freidah
//
// Extends handler_test.go, which only exercises the auth and input-validation
// paths, with tests that route through a real BackendManager backed by the
// shared union store mock. Covers status, cleanup queue, replication,
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
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// newTestHandlerWithManager returns a Handler backed by a real BackendManager
// wrapping the generated union store mock. Suitable for exercising handlers that reach
// into manager or cb-store methods. Encryptor, rawStore, and reconciler are
// nil  -  handlers that require them should assert the documented nil-handling
// behaviour rather than the happy path.
func newTestHandlerWithManager(t *testing.T) *Handler {
	t.Helper()
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(mock)
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
	})
	mgr := proxytest.NewManager(t, mock, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: map[string]backend.ObjectBackend{},
			Order:    []string{},
		},
		Policies: proxy.PolicyConfig{
			RoutingStrategy: config.RoutingPack,
		},
		Operations: proxy.OperationalDeps{
			Metrics: mock,
		},
	})
	workers := proxytest.BuildWorkers(mgr, mock)
	// Empty reloadable configs so Replicator.Config()/Scrubber.Config() return
	// sentinel states the handlers can interpret.
	workers.Replicator.SetConfig(&config.ReplicationConfig{Factor: 1})
	workers.OverReplicationCleaner.SetConfig(&config.ReplicationConfig{Factor: 1})

	var lv slog.LevelVar
	lv.Set(slog.LevelInfo)
	svc := testOps(mgr, workers, mock, nil)
	return &Handler{
		log:          slog.Default().With(logfmt.Component("admin")),
		backendOps:   mgr,
		dashboardOps: dashboard.New(mock, mgr.Runtime().Usage(), nil, mgr.Runtime(), mgr.Drain()),
		objects:      svc.Objects,
		integrity:    svc.Integrity,
		replication:  svc.Replication,
		rebalance:    svc.Rebalance,
		encryption:   svc.Encryption,
		drain:        mgr.Drain(),
		lifecycle:    mock,
		dbHealthy:    cb.IsHealthy,
		cleanup:      mock,
		token:        "test-token",
		logLevel:     &lv,
	}
}

// objectsOver builds an object operations service over one store mock, for the
// handlers that only read object metadata and never move bytes.
func objectsOver(t *testing.T, store ops.ObjectStore) *ops.Objects {
	t.Helper()
	return ops.NewObjects(ops.ObjectsDeps{
		Objects: opstest.NewMockObjectAPI(gomock.NewController(t)),
		Store:   store,
		Config:  ops.NewConfigStore(&config.Config{}),
	})
}

// testOps assembles the operations layer over the fixture's real manager and
// workers, so a handler test drives the same code the process wires.
func testOps(mgr *proxy.BackendManager, workers *proxytest.Workers, store core.MetadataStore, enc *encryption.Encryptor) *ops.Services {
	return ops.New(&ops.Deps{
		Objects:    mgr.Objects(),
		Store:      store,
		Encryptor:  enc,
		EncStore:   store,
		Runtime:    mgr.Runtime(),
		BackendOps: mgr,
		Replicator: workers.Replicator,
		OverRep:    workers.OverReplicationCleaner,
		Rebalancer: workers.Rebalancer,
		Scrubber:   workers.Scrubber,
		Cfg:        &config.Config{},
	})
}

// doAuth builds a request pre-populated with the correct admin token.
func doAuth(method, path string, body string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
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

// TestHandleCleanupQueue_ItemShape pins the wire shape of a pending cleanup:
// snake_case names shared with the dead-letter listing, and claim fields
// omitted while no worker holds the row.
func TestHandleCleanupQueue_ItemShape(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	cleanupMock := storetest.NewMockCleanupStore(gomock.NewController(t))
	cleanupMock.EXPECT().CleanupQueueDepth(gomock.Any()).Return(int64(2), nil).Times(1)
	cleanupMock.EXPECT().GetPendingCleanups(gomock.Any(), gomock.Any()).Return([]core.CleanupItem{{
		ID:          7,
		BackendName: "minio-1",
		ObjectKey:   "photos/cat.jpg",
		Reason:      "delete_failed",
		SizeBytes:   4096,
		Attempts:    2,
	}}, nil).Times(1)
	h.cleanup = cleanupMock
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/cleanup-queue", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.CleanupQueueResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Depth != 2 || len(resp.Items) != 1 {
		t.Fatalf("got depth=%d items=%d, want 2/1", resp.Depth, len(resp.Items))
	}
	got := resp.Items[0]
	want := adminapi.CleanupQueueItem{
		ID: 7, Backend: "minio-1", ObjectKey: "photos/cat.jpg",
		Reason: "delete_failed", SizeBytes: 4096, Attempts: 2,
	}
	if got != want {
		t.Errorf("item = %+v, want %+v", got, want)
	}
	// An unclaimed row must not emit null claim fields.
	if body := w.Body.String(); strings.Contains(body, "claimed_at") || strings.Contains(body, "claimed_by") {
		t.Errorf("unclaimed item emitted claim fields: %s", body)
	}
}

// TestHandleObjectLocations_Happy covers the key-resolution path. The mock
// is pre-seeded so GetAllObjectLocations returns a non-empty slice.
func TestHandleObjectLocations_Happy(t *testing.T) {
	t.Parallel()
	mock := storetest.NewMockObjectStore(gomock.NewController(t))
	mock.EXPECT().GetAllObjectLocations(gomock.Any(), "foo").Return([]core.ObjectLocation{{
		ObjectKey:     "foo",
		BackendName:   "b1",
		Encrypted:     true,
		KeyID:         "kid-1",
		EncryptionKey: []byte("super-secret-raw-key"),
	}}, nil).Times(1)
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	var lv slog.LevelVar
	h := &Handler{log: slog.Default().With(logfmt.Component("admin")), dbHealthy: cb.IsHealthy, objects: objectsOver(t, mock), token: "test-token", logLevel: &lv}
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/object-locations?key=foo", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp adminapi.ObjectLocationsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if resp.Key != "foo" || len(resp.Locations) != 1 {
		t.Fatalf("resp = %+v, want key=foo with 1 location", resp)
	}
	if resp.Locations[0].Backend != "b1" || resp.Locations[0].KeyID != "kid-1" {
		t.Errorf("location = %+v, want backend=b1 key_id=kid-1", resp.Locations[0])
	}

	// The raw envelope key must never cross the wire.
	if strings.Contains(w.Body.String(), "super-secret-raw-key") || strings.Contains(w.Body.String(), "encryption_key") {
		t.Errorf("response leaked the encryption key: %s", w.Body.String())
	}
}

// TestHandleObjectLocations_NotFound covers the 500 path when the store
// returns ErrObjectNotFound. The handler does not distinguish not-found from
// any other store error, so an unknown key is a 500 rather than a 404.
func TestHandleObjectLocations_NotFound(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	objects := storetest.NewMockObjectStore(gomock.NewController(t))
	objects.EXPECT().GetAllObjectLocations(gomock.Any(), "ghost").Return(nil, core.ErrObjectNotFound).Times(1)
	h.objects = objectsOver(t, objects)
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
	var resp adminapi.ScrubResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != "skipped" || resp.Reason == "" {
		t.Errorf("got status=%q reason=%q, want skipped with a reason", resp.Status, resp.Reason)
	}
	// The skipped branch reports the counters as zero rather than omitting
	// them, so both branches of the endpoint carry one shape.
	if resp.Checked != 0 || resp.Failed != 0 {
		t.Errorf("got checked=%d failed=%d, want both zero", resp.Checked, resp.Failed)
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

// newRotateEncryptionKeyHandler returns a handler wired with a real
// encryptor and an empty EncryptionAdmin, ready to exercise the
// request-body validation paths of /admin/api/rotate-encryption-key.
func newRotateEncryptionKeyHandler(t *testing.T) *Handler {
	t.Helper()
	h := newTestHandlerWithManager(t)
	encryptionWith(t, h, testEncryptor(t), emptyEncryptionStore(t))
	return h
}

// testEncryptor builds a real encryptor over the local config-key provider,
// for the paths that must get past the encryption-disabled guard.
func testEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	provider, err := encryption.NewConfigKeyProvider(
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-key")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

// TestHandleRotateEncryptionKey_BodyValidation covers the request-body
// validation paths past the nil-encryptor guard: malformed JSON should be
// rejected by DecodeJSONBody, and an empty old_key_id should be rejected
// by the explicit field check.
func TestHandleRotateEncryptionKey_BodyValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"invalid json", `{not json`, "invalid request body"},
		{"empty old_key_id", `{"old_key_id":""}`, "old_key_id is required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRotateEncryptionKeyHandler(t)
			mux := http.NewServeMux()
			h.Register(mux)

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/rotate-encryption-key", tc.body))

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			var resp map[string]string
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if resp["error"] != tc.wantMsg {
				t.Errorf("error = %q, want %q", resp["error"], tc.wantMsg)
			}
		})
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
	cleanupMock := storetest.NewMockCleanupStore(gomock.NewController(t))
	cleanupMock.EXPECT().CleanupQueueDepth(gomock.Any()).Return(int64(0), errors.New("db down")).Times(1)
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
	cleanupMock := storetest.NewMockCleanupStore(gomock.NewController(t))
	cleanupMock.EXPECT().CleanupQueueDepth(gomock.Any()).Return(int64(5), nil).Times(1)
	cleanupMock.EXPECT().GetPendingCleanups(gomock.Any(), gomock.Any()).Return(nil, errors.New("query failed")).Times(1)
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

func (f *flushUsageFailingOps) FlushUsage(_ context.Context) error {
	return errors.New("flush failed")
}
func (f *flushUsageFailingOps) ReconcileUsage(ctx context.Context) (map[string]int64, error) {
	return f.inner.ReconcileUsage(ctx)
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

// TestHandleStatus_DashboardError covers the error branch where the
// backend ops dashboard call fails.
func TestHandleStatus_DashboardError(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	h.dashboardOps = newDashboardOps(t, nil, errors.New("dashboard unavailable"))
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
	mock := storetest.NewMockObjectStore(gomock.NewController(t))
	mock.EXPECT().GetAllObjectLocations(gomock.Any(), "foo").Return(nil, errors.New("query failed")).Times(1)
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	var lv slog.LevelVar
	h := &Handler{log: slog.Default().With(logfmt.Component("admin")), dbHealthy: cb.IsHealthy, objects: objectsOver(t, mock), token: "test-token", logLevel: &lv}
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

// TestHandleDrainProgress_InactiveShape pins the drain-progress wire shape for
// a backend that is not draining. The handler converts drain.Progress at its
// boundary, so this is what keeps an internal field rename from reaching the
// API.
func TestHandleDrainProgress_InactiveShape(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/backends/b1/drain", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.DrainProgressResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Active {
		t.Errorf("active = true, want false for a backend with no drain")
	}
	if resp.ObjectsRemaining != 0 || resp.BytesRemaining != 0 || resp.ObjectsMoved != 0 {
		t.Errorf("counters = %+v, want all zero", resp)
	}
	// Error is omitempty, so a clean snapshot must not carry the key at all.
	if body := w.Body.String(); strings.Contains(body, "error") {
		t.Errorf("clean progress emitted an error field: %s", body)
	}
}

// TestHandleRemoveBackend_AcknowledgementShape pins the acknowledgement the
// backend mutation endpoints share: the prose status plus the backend acted on.
func TestHandleRemoveBackend_AcknowledgementShape(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/backends/b1", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.BackendOperationResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "backend removed" || resp.Backend != "b1" {
		t.Errorf("got status=%q backend=%q, want {backend removed b1}", resp.Status, resp.Backend)
	}
}

// TestHandleRemoveBackend_PurgeTwoPhaseShapes walks the destructive purge flow
// end to end: the preview returns a confirmation token, and replaying it
// executes the purge. Both responses are typed, and the second reuses the same
// acknowledgement DTO as every other backend mutation.
func TestHandleRemoveBackend_PurgeTwoPhaseShapes(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/backends/b1?purge=true", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var preview adminapi.RemoveBackendPreview
	if err := json.NewDecoder(w.Body).Decode(&preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.Status != "confirmation required" || preview.Backend != "b1" {
		t.Errorf("got status=%q backend=%q, want {confirmation required b1}", preview.Status, preview.Backend)
	}
	if preview.ConfirmToken == "" || preview.ExpiresIn <= 0 {
		t.Fatalf("preview must carry a token and a TTL: %+v", preview)
	}

	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, doAuth(http.MethodDelete,
		"/admin/api/backends/b1?purge=true&confirm="+preview.ConfirmToken, ""))
	if w2.Code != http.StatusOK {
		t.Fatalf("purge status = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	var resp adminapi.BackendOperationResponse
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode purge: %v", err)
	}
	if resp.Status != "backend purged" || resp.Backend != "b1" {
		t.Errorf("got status=%q backend=%q, want {backend purged b1}", resp.Status, resp.Backend)
	}
}

// TestHandleStartDrain_AcknowledgementShape covers the accepted branch, which
// needs a manager that actually knows the backend -- the shared fixture
// registers none, so StartDrain there always rejects. Cancels the drain on the
// way out so the background pass does not outlive the test.
func TestHandleStartDrain_AcknowledgementShape(t *testing.T) {
	t.Parallel()
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(mock)
	mgr := proxytest.NewManager(t, mock, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()},
			Order:    []string{"b1"},
		},
		Policies:   proxy.PolicyConfig{RoutingStrategy: config.RoutingPack},
		Operations: proxy.OperationalDeps{Metrics: mock},
	})
	var lv slog.LevelVar
	lv.Set(slog.LevelInfo)
	h := &Handler{
		log:          slog.Default().With(logfmt.Component("admin")),
		backendOps:   mgr,
		dashboardOps: dashboard.New(mock, mgr.Runtime().Usage(), nil, mgr.Runtime(), mgr.Drain()),
		drain:        mgr.Drain(),
		lifecycle:    mock,
		token:        "test-token",
		logLevel:     &lv,
	}
	t.Cleanup(func() { _ = h.drain.CancelDrain("b1") })

	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/backends/b1/drain", ""))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.BackendOperationResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "drain started" || resp.Backend != "b1" {
		t.Errorf("got status=%q backend=%q, want {drain started b1}", resp.Status, resp.Backend)
	}
}
