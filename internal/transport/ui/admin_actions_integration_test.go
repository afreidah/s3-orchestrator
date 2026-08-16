// -------------------------------------------------------------------------------
// UI Admin Actions Integration Tests
//
// Author: Alex Freidah
//
// Drives every handleAPI* wrapper through a real operations layer so the
// closure body that calls into the operation is actually executed. The
// operations are configured into the documented "skipped" shape (replication
// factor 1, encryptor nil, integrity disabled) so each one short-circuits
// without touching real backends  -  exactly the path the dashboard's banner
// relies on.
// -------------------------------------------------------------------------------

package ui

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// newOpsForTest builds the operations layer against a mock store and an empty
// backend set. opts mutates the resulting BackendManager and workers so
// individual tests can flip the relevant skipped-vs-happy guards.
func newOpsForTest(t testing.TB, opts ...func(*proxy.BackendManager, *proxytest.Workers)) *ops.Services {
	t.Helper()
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	// The handler drives real workers, whose passes read the ledger. These are
	// incidental to the wrapper-closure behaviour under test.
	mock.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mock.EXPECT().GetUnderReplicatedObjectsExcluding(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mock.EXPECT().GetOverReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mock.EXPECT().CountOverReplicatedObjects(gomock.Any(), gomock.Any()).Return(int64(0), nil).AnyTimes()
	mock.EXPECT().CountUnencryptedLocations(gomock.Any()).Return(int64(0), nil).AnyTimes()
	mock.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mock.EXPECT().GetQuotaStats(gomock.Any()).Return(map[string]core.QuotaStat{}, nil).AnyTimes()
	mock.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	mock.EXPECT().GetUnverifiedObjectCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	mock.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	mock.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.UsageStat{}, nil).AnyTimes()
	mock.EXPECT().GetLeastRecentlyScrubbedObjects(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mock.EXPECT().OldestUnverifiedAge(gomock.Any()).Return(time.Duration(0), int64(0), nil).AnyTimes()
	mock.EXPECT().GetObjectsWithoutHash(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
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
	workers.Replicator.SetConfig(&config.ReplicationConfig{Factor: 1})
	workers.OverReplicationCleaner.SetConfig(&config.ReplicationConfig{Factor: 1})
	for _, opt := range opts {
		opt(mgr, workers)
	}

	return testOps(mgr, workers, mock)
}

// testOps assembles the operations layer over a manager and its workers, the
// same wiring the composition root performs.
func testOps(mgr *proxy.BackendManager, workers *proxytest.Workers, store core.MetadataStore) *ops.Services {
	return ops.New(&ops.Deps{
		Objects:    mgr.Objects(),
		Store:      store,
		EncStore:   store,
		Runtime:    mgr.Runtime(),
		BackendOps: mgr,
		Replicator: workers.Replicator,
		OverRep:    workers.OverReplicationCleaner,
		Rebalancer: workers.Rebalancer,
		Scrubber:   workers.Scrubber,
		Cfg:        &config.Config{Buckets: []config.BucketConfig{{Name: "test-bucket"}}},
	})
}

// newActionsHandler builds a UI handler whose operations are the ones under
// test. Every operation resolves to the skipped branch unless opts say
// otherwise.
func newActionsHandler(t testing.TB, opts ...func(*proxy.BackendManager, *proxytest.Workers)) *Handler {
	t.Helper()
	svc := newOpsForTest(t, opts...)
	return &Handler{
		log:         slog.Default(),
		objects:     svc.Objects,
		integrity:   svc.Integrity,
		replication: svc.Replication,
		rebalance:   svc.Rebalance,
		encryption:  svc.Encryption,
	}
}

// TestHandleAPIReplicate_HappyPathReturnsCount asserts that when the
// admin handler is configured to actually run (factor > 1) the wrapper
// closure surfaces the CopiesCreated count via the status endpoint
// rather than the skipped reason. Drives the "non-skipped return" line
// of every wrapper closure since the structural shape is identical
// across the four operations; covering one is enough.
func TestHandleAPIReplicate_HappyPathReturnsCount(t *testing.T) {
	t.Parallel()
	h := newActionsHandler(t, func(_ *proxy.BackendManager, workers *proxytest.Workers) {
		workers.Replicator.SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 10})
	})

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

// TestHandleAPIDownload_StreamsObject asserts the dashboard's download serves
// the object bytes with the headers a browser needs to save the file.
func TestHandleAPIDownload_StreamsObject(t *testing.T) {
	t.Parallel()
	api := opstest.NewMockObjectAPI(gomock.NewController(t))
	payload := []byte("hello world")
	api.EXPECT().GetObject(gomock.Any(), "test-bucket/dir/file.txt", "").
		Return(&backend.GetObjectResult{
			Body: io.NopCloser(bytes.NewReader(payload)),
			Size: int64(len(payload)),
		}, nil).Times(1)

	h := &Handler{
		log: slog.Default(),
		objects: ops.NewObjects(ops.ObjectsDeps{
			Objects: api,
			Store:   storetest.NewMockObjectStore(gomock.NewController(t)),
			Config:  ops.NewConfigStore(&config.Config{Buckets: []config.BucketConfig{{Name: "test-bucket"}}}),
		}),
	}

	w := httptest.NewRecorder()
	h.handleAPIDownload(w, httptest.NewRequestWithContext(context.Background(),
		http.MethodGet, "/api/download?key=test-bucket/dir/file.txt", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Errorf("body = %q, want %q", w.Body.String(), payload)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="file.txt"` {
		t.Errorf("Content-Disposition = %q, want the base filename", cd)
	}
	if cl := w.Header().Get("Content-Length"); cl != "11" {
		t.Errorf("Content-Length = %q, want 11", cl)
	}
}

// TestHandleAPIEncryptExisting_ReportsCounts asserts the encrypt-existing
// wrapper surfaces the pass counts once an encryptor is configured, rather
// than only the skipped reason.
func TestHandleAPIEncryptExisting_ReportsCounts(t *testing.T) {
	t.Parallel()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(store)
	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-key")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	h := &Handler{
		log: slog.Default(),
		encryption: ops.NewEncryption(ops.EncryptionDeps{
			Encryptor:  enc,
			Store:      store,
			Runtime:    opstest.NewMockRuntimeOps(gomock.NewController(t)),
			BackendOps: opstest.NewMockBackendOps(gomock.NewController(t)),
		}),
	}

	w := httptest.NewRecorder()
	h.handleAPIEncryptExisting(w, httptest.NewRequestWithContext(context.Background(),
		http.MethodPost, "/api/encrypt-existing", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	res := waitForResult(t, h, "encrypt-existing")
	if !res.OK || res.Skipped != "" {
		t.Errorf("result = %+v, want a completed pass with no skip reason", res)
	}
}

// TestIntegrityActions_ReportCountsWhenEnabled asserts the scrub and backfill
// wrappers surface counts once verification is on, rather than only the
// skipped reason the default fixture produces.
func TestIntegrityActions_ReportCountsWhenEnabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		opName  string
		trigger func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{"scrub", (*Handler).handleAPIScrub},
		{"backfill-checksums", (*Handler).handleAPIBackfillChecksums},
	}

	for _, tc := range cases {
		t.Run(tc.opName, func(t *testing.T) {
			t.Parallel()
			h := newActionsHandler(t, func(mgr *proxy.BackendManager, _ *proxytest.Workers) {
				mgr.SetIntegrityConfig(&config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 10})
			})

			w := httptest.NewRecorder()
			tc.trigger(h, w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/"+tc.opName, nil))
			if w.Code != http.StatusAccepted {
				t.Fatalf("trigger status = %d, want 202", w.Code)
			}

			res := waitForResult(t, h, tc.opName)
			if !res.OK || res.Skipped != "" {
				t.Errorf("result = %+v, want a completed pass with no skip reason", res)
			}
		})
	}
}

// TestHandleAPICleanExcess_ReportsRemovedCount asserts the surplus cleanup
// reports what it removed once the factor makes the pass meaningful.
func TestHandleAPICleanExcess_ReportsRemovedCount(t *testing.T) {
	t.Parallel()
	h := newActionsHandler(t, func(_ *proxy.BackendManager, workers *proxytest.Workers) {
		workers.OverReplicationCleaner.SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 10})
	})

	w := httptest.NewRecorder()
	h.handleAPICleanExcess(w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/clean-excess", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	res := waitForResult(t, h, opCleanExcess)
	if !res.OK || res.Skipped != "" {
		t.Errorf("result = %+v, want a completed pass with no skip reason", res)
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
			h := newActionsHandler(t)

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
