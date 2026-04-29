// -------------------------------------------------------------------------------
// UI Admin Actions Integration Tests
//
// Author: Alex Freidah
//
// Drives every handleAPI* wrapper through a real *admin.Handler so the
// closure body that calls into the admin operation is actually executed.
// The admin handler is configured into the documented "skipped" shape
// (replication factor 1, encryptor nil, integrity disabled) so each
// op short-circuits without touching real backends — exactly the path
// the dashboard's banner relies on.
// -------------------------------------------------------------------------------

package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
)

// newSkippedAdminHandler builds an *admin.Handler whose operations all
// resolve to the documented "skipped" branch: factor-1 replication, no
// encryptor, no integrity config. Mirrors the fixture in admin's tests
// since the helper there is unexported.
func newSkippedAdminHandler(t *testing.T) *admin.Handler {
	t.Helper()
	mock := &testutil.MockStore{}
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{},
		Stores:          proxy.StoresFromMock(mock),
		Dashboard:       mock,
		Metrics:         mock,
		Order:           []string{},
		RoutingStrategy: config.RoutingPack,
	})
	mgr.Replicator.SetConfig(&config.ReplicationConfig{Factor: 1})
	mgr.OverReplicationCleaner.SetConfig(&config.ReplicationConfig{Factor: 1})

	return admin.New(&admin.Deps{
		BackendOps: mgr,
		Replicator: mgr.Replicator,
		OverRep:    mgr.OverReplicationCleaner,
		Drain:      mgr.DrainManager,
		Scrubber:   mgr.Scrubber,
		Lifecycle:  mock,
		DBCB:       cb,
		Objects:    mock,
		Cleanup:    mock,
		Token:      "test-token",
	})
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
			h := &Handler{adminHandler: newSkippedAdminHandler(t)}

			triggerReq := httptest.NewRequest(http.MethodPost, tc.triggerPath, nil)
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

			statusReq := httptest.NewRequest(http.MethodGet, tc.statusPath, nil)
			statusW := httptest.NewRecorder()
			tc.status(h, statusW, statusReq)
			body := decodeBody(t, statusW)
			if body["status"] != "skipped" {
				t.Errorf("op %q status body = %v, want status=skipped", tc.opName, body)
			}
		})
	}
}