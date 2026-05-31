// -------------------------------------------------------------------------------
// UI Admin Actions Integration Tests
//
// Author: Alex Freidah
//
// Drives every handleAPI* wrapper through a real *admin.Handler so the
// closure body that calls into the admin operation is actually executed.
// The admin handler is configured into the documented "skipped" shape
// (replication factor 1, encryptor nil, integrity disabled) so each
// op short-circuits without touching real backends  -  exactly the path
// the dashboard's banner relies on.
// -------------------------------------------------------------------------------

package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
)

// newAdminHandlerForTest builds an *admin.Handler against a mock store
// and an empty backend set. opts mutates the resulting BackendManager so
// individual tests can flip the relevant skipped-vs-happy guards before
// the handler is returned.
func newAdminHandlerForTest(t testing.TB, opts ...func(*proxy.BackendManager, *proxytest.Workers)) *admin.Handler {
	t.Helper()
	mock := testutil.NewMockStore(t)
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	mgr := proxytest.NewManager(t, &proxy.BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{},
		Stores:          mock,
		Dashboard:       mock,
		Metrics:         mock,
		Order:           []string{},
		RoutingStrategy: config.RoutingPack,
	})
	workers := proxytest.BuildWorkers(mgr, mock)
	workers.Replicator.SetConfig(&config.ReplicationConfig{Factor: 1})
	workers.OverReplicationCleaner.SetConfig(&config.ReplicationConfig{Factor: 1})
	for _, opt := range opts {
		opt(mgr, workers)
	}

	lv := new(slog.LevelVar)
	return admin.New(&admin.Deps{
		BackendOps: mgr,
		Replicator: workers.Replicator,
		OverRep:    workers.OverReplicationCleaner,
		Drain:      mgr.Drain(),
		Scrubber:   workers.Scrubber,
		Lifecycle:  mock,
		DBHealthy:  cb.IsHealthy,
		Encryption: mock,
		Objects:    mock,
		Cleanup:    mock,
		Token:      "test-token",
		LogLevel:   lv,
	})
}

// newSkippedAdminHandler is the most common shape: every op resolves to
// the skipped branch.
func newSkippedAdminHandler(t testing.TB) *admin.Handler {
	t.Helper()
	return newAdminHandlerForTest(t)
}

// TestHandleAPIReplicate_HappyPathReturnsCount asserts that when the
// admin handler is configured to actually run (factor > 1) the wrapper
// closure surfaces the CopiesCreated count via the status endpoint
// rather than the skipped reason. Drives the "non-skipped return" line
// of every wrapper closure since the structural shape is identical
// across the four operations; covering one is enough.
func TestHandleAPIReplicate_HappyPathReturnsCount(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default(), adminHandler: newAdminHandlerForTest(t, func(_ *proxy.BackendManager, workers *proxytest.Workers) {
		workers.Replicator.SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 10})
	})}

	triggerReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/replicate", nil)
	triggerW := httptest.NewRecorder()
	h.handleAPIReplicate(triggerW, triggerReq)
	if triggerW.Code != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want %d", triggerW.Code, http.StatusAccepted)
	}

	res := waitForResult(t, h, "replicate")
	if !res.OK || res.Skipped != "" {
		t.Errorf("result = %+v, want OK and no Skipped reason", res)
	}
	if res.Count != 0 {
		t.Errorf("Count = %d, want 0 (empty store)", res.Count)
	}

	statusReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/replicate/status", nil)
	statusW := httptest.NewRecorder()
	h.handleAPIReplicateStatus(statusW, statusReq)
	body := decodeBody(t, statusW)
	if body["status"] != "done" {
		t.Errorf("status body = %v, want status=done", body)
	}
	if body["copies_created"] != float64(0) {
		t.Errorf("copies_created = %v, want 0", body["copies_created"])
	}
}

// TestAdminActionWrappers_RouteIntoAdmin asserts that each trigger
// wrapper actually invokes its admin counterpart through the real
// goroutine path and surfaces the skipped reason on the status endpoint.
// This covers the closure body of every handleAPI* wrapper, which the
// nil-handler smoke tests cannot reach because they short-circuit on the
// 503 guard.
func TestAdminActionWrappers_RouteIntoAdmin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		opName      string
		triggerPath string
		statusPath  string
		trigger     func(*Handler, http.ResponseWriter, *http.Request)
		status      func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{"replicate", "/api/replicate", "/api/replicate/status",
			(*Handler).handleAPIReplicate, (*Handler).handleAPIReplicateStatus},
		{"scrub", "/api/scrub", "/api/scrub/status",
			(*Handler).handleAPIScrub, (*Handler).handleAPIScrubStatus},
		{"backfill-checksums", "/api/backfill-checksums", "/api/backfill-checksums/status",
			(*Handler).handleAPIBackfillChecksums, (*Handler).handleAPIBackfillChecksumsStatus},
		{"encrypt-existing", "/api/encrypt-existing", "/api/encrypt-existing/status",
			(*Handler).handleAPIEncryptExisting, (*Handler).handleAPIEncryptExistingStatus},
	}

	for _, tc := range cases {
		t.Run(tc.opName, func(t *testing.T) {
			t.Parallel()
			h := &Handler{log: slog.Default(), adminHandler: newSkippedAdminHandler(t)}

			triggerReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, tc.triggerPath, nil)
			triggerW := httptest.NewRecorder()
			tc.trigger(h, triggerW, triggerReq)
			if triggerW.Code != http.StatusAccepted {
				t.Fatalf("trigger status = %d, want %d (body=%s)",
					triggerW.Code, http.StatusAccepted, triggerW.Body.String())
			}

			res := waitForResult(t, h, tc.opName)
			if res.Skipped == "" {
				t.Errorf("op %q result has empty Skipped reason; want non-empty", tc.opName)
			}

			statusReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tc.statusPath, nil)
			statusW := httptest.NewRecorder()
			tc.status(h, statusW, statusReq)
			body := decodeBody(t, statusW)
			if body["status"] != "skipped" {
				t.Errorf("op %q status body = %v, want status=skipped", tc.opName, body)
			}
		})
	}
}
