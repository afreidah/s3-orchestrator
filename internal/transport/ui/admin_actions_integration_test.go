// -------------------------------------------------------------------------------
// UI Admin Actions Integration Tests
//
// Author: Alex Freidah
//
// Drives every dashboard action through a real operations layer so the pass
// it builds is actually run. By default the operations are configured off
// (replication factor 1, encryptor nil, integrity disabled), so each one
// declines without touching real backends, which is the path the dashboard's
// banner relies on.
// -------------------------------------------------------------------------------

package ui

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/mock/gomock"

	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newOpsForTest builds the operations layer against a mock store and an empty
// backend set. opts mutates the resulting stack and workers so
// individual tests can flip the relevant skipped-vs-happy guards.
func newOpsForTest(t testing.TB, opts ...func(*proxytest.Stack, *proxytest.Workers)) *ops.Services {
	t.Helper()
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	// The handler drives real workers, whose passes read the ledger. These are
	// incidental to the wrapper-closure behaviour under test.
	mock.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mock.EXPECT().GetOverReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mock.EXPECT().CountReplicationBacklog(gomock.Any(), gomock.Any()).Return(core.ReplicationBacklog{}, nil).AnyTimes()
	mock.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mock.EXPECT().GetQuotaStats(gomock.Any()).Return(map[string]core.QuotaStat{}, nil).AnyTimes()
	mock.EXPECT().LedgerStats(gomock.Any()).Return(core.LedgerStats{}, nil).AnyTimes()
	mock.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	mock.EXPECT().GetUsageSince(gomock.Any(), gomock.Any()).Return(map[string]core.WorkloadUsage{}, nil).AnyTimes()
	mock.EXPECT().GetLeastRecentlyScrubbedObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mock.EXPECT().ListCopiesByForm(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	// An operator-triggered scrub runs under the advisory lock, so the dashboard
	// only reaches the scrubber when the lock is granted.
	mock.EXPECT().WithAdvisoryLock(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(ctx context.Context, _ int64, fn func(context.Context) error) (bool, error) {
			return true, fn(ctx)
		}).AnyTimes()
	st := proxytest.New(t, mock, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{
			Backends:        map[string]backend.ObjectBackend{},
			Order:           []string{},
			RoutingStrategy: config.RoutingPack,
		}),
	})
	workers := proxytest.BuildWorkers(st, mock)
	st.Config.Update(func(c *config.Config) {
		c.Buckets = []config.BucketConfig{{Name: "test-bucket"}}
		c.Replication.Factor = 1
	})
	for _, opt := range opts {
		opt(st, workers)
	}

	return testOps(st, workers, mock)
}

// testOps assembles the operations layer over a stack and its workers, the
// same wiring the composition root performs.
func testOps(st *proxytest.Stack, workers *proxytest.Workers, store storetest.MetadataStore) *ops.Services {
	return ops.New(&ops.Deps{
		Objects:    st.Objects,
		Store:      store,
		EncStore:   store,
		CompStore:  store,
		Locker:     store,
		Mover:      st.Coord,
		Replicator: workers.Replicator,
		OverRep:    workers.OverReplicationCleaner,
		Backlog:    store,
		Rebalancer: workers.Rebalancer,
		Scrubber:   workers.Scrubber,
		Declared:   declaredFrom(&config.Config{Buckets: []config.BucketConfig{{Name: "test-bucket"}}}),
		Config:     st.Config,
	})
}

// newActionsHandler builds a UI handler whose operations are the ones under
// test. Every operation resolves to the skipped branch unless opts say
// otherwise.
func newActionsHandler(t testing.TB, opts ...func(*proxytest.Stack, *proxytest.Workers)) *Handler {
	t.Helper()
	return &Handler{log: slog.Default(), d: Deps{Ops: newOpsForTest(t, opts...)}}
}

// assertRan fails unless the action finished with the pass having run.
func assertRan(t *testing.T, res *admin.Report) {
	t.Helper()
	if res.Result != adminstream.OutcomeOK {
		t.Errorf("result = %+v, want a completed pass", res)
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestHandleAPIReplicate_HappyPathReturnsCount asserts that with replication
// enabled (factor > 1) the status endpoint reports the CopiesCreated count
// rather than a skipped reason.
func TestHandleAPIReplicate_HappyPathReturnsCount(t *testing.T) {
	t.Parallel()
	h := newActionsHandler(t, func(st *proxytest.Stack, _ *proxytest.Workers) {
		st.Config.Update(func(c *config.Config) { c.Replication = config.ReplicationConfig{Factor: 2, BatchSize: 10} })
	})

	triggerReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/replicate", nil)
	triggerW := httptest.NewRecorder()
	serveRoute(h, triggerW, triggerReq)
	if triggerW.Code != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want %d", triggerW.Code, http.StatusAccepted)
	}

	assertRan(t, waitForResult(t, h, "/api/replicate"))

	statusReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/replicate/status", nil)
	statusW := httptest.NewRecorder()
	serveRoute(h, statusW, statusReq)
	body := decodeBody(t, statusW)
	result, _ := body["result"].(map[string]any)
	if body["status"] != adminstream.OutcomeOK || result["copies_created"] != float64(0) {
		t.Errorf("status body = %v, want ok with copies_created 0 (empty store)", body)
	}
}

// TestHandleAPIDownload_StreamsObject asserts the dashboard's download serves
// the object bytes with the headers a browser needs to save the file.
func TestHandleAPIDownload_StreamsObject(t *testing.T) {
	t.Parallel()
	api := opstest.NewMockObjectAPI(gomock.NewController(t))
	payload := []byte("hello world")
	api.EXPECT().GetObject(gomock.Any(), "test-bucket/dir/file.txt", "").
		Return(&object.GetResult{
			GetObjectResult: &backend.GetObjectResult{
				Body: io.NopCloser(bytes.NewReader(payload)),
				Size: int64(len(payload)),
			},
		}, nil).Times(1)

	h := &Handler{log: slog.Default(), d: Deps{
		Ops: &ops.Services{Objects: ops.NewObjects(ops.ObjectsDeps{
			Objects: api,
			Store:   storetest.NewMockObjectStore(gomock.NewController(t)),
			Buckets: declaredFrom(&config.Config{Buckets: []config.BucketConfig{{Name: "test-bucket"}}}),
		})},
	}}

	reg, err := auth.NewBucketRegistry(&provisioning.View{Users: []provisioning.User{{
		ID: "reader", Name: "reader", Grants: map[string]core.PermissionSet{"test-bucket": core.PermRead},
	}}})
	if err != nil {
		t.Fatalf("NewBucketRegistry: %v", err)
	}
	reader, _ := reg.UserByID("reader")
	w := httptest.NewRecorder()
	h.handleAPIDownload(w, httptest.NewRequestWithContext(withUser(context.Background(), reader),
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
	provider, err := encryption.NewStaticKeyProvider(make([]byte, config.KeySize))
	if err != nil {
		t.Fatalf("NewStaticKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	h := &Handler{log: slog.Default(), d: Deps{
		Ops: &ops.Services{Encryption: ops.NewEncryption(ops.EncryptionDeps{
			Encryptor: enc,
			Store:     store,
			Mover:     opstest.NewMockMover(gomock.NewController(t)),
		})},
	}}

	w := httptest.NewRecorder()
	serveRoute(h, w, httptest.NewRequestWithContext(context.Background(),
		http.MethodPost, "/api/encrypt-existing", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	assertRan(t, waitForResult(t, h, "/api/encrypt-existing"))
}

// TestIntegrityActions_ReportCountsWhenEnabled asserts the scrub and backfill
// wrappers surface counts once verification is on, rather than only the
// skipped reason the default fixture produces.
func TestIntegrityActions_ReportCountsWhenEnabled(t *testing.T) {
	t.Parallel()

	for _, opName := range []string{"scrub", "backfill-checksums"} {
		t.Run(opName, func(t *testing.T) {
			t.Parallel()
			h := newActionsHandler(t, func(st *proxytest.Stack, _ *proxytest.Workers) {
				st.Config.Update(func(c *config.Config) { c.Integrity = config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 10} })
			})

			w := httptest.NewRecorder()
			serveRoute(h, w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/"+opName, nil))
			if w.Code != http.StatusAccepted {
				t.Fatalf("trigger status = %d, want 202", w.Code)
			}

			assertRan(t, waitForResult(t, h, "/api/"+opName))
		})
	}
}

// TestHandleAPICleanExcess_ReportsRemovedCount asserts the surplus cleanup
// reports what it removed once the factor makes the pass meaningful.
func TestHandleAPICleanExcess_ReportsRemovedCount(t *testing.T) {
	t.Parallel()
	h := newActionsHandler(t, func(st *proxytest.Stack, _ *proxytest.Workers) {
		st.Config.Update(func(c *config.Config) { c.Replication = config.ReplicationConfig{Factor: 2, BatchSize: 10} })
	})

	w := httptest.NewRecorder()
	serveRoute(h, w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/clean-excess", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	assertRan(t, waitForResult(t, h, "/api/clean-excess"))
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// partialReplication builds a replication service whose cycles both report
// work left unfinished, which a real fleet only produces against failing
// backends.
func partialReplication(t *testing.T, created, removed, failed int) *ops.Replication {
	t.Helper()
	ctrl := gomock.NewController(t)
	repl := opstest.NewMockReplicator(ctrl)
	repl.EXPECT().Replicate(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(worker.ReplicationSummary{
			Succeeded: created, Failed: failed,
			CopiesCreated: created,
		}, nil).AnyTimes()

	over := opstest.NewMockOverReplicationCleaner(ctrl)
	over.EXPECT().Clean(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(worker.OverReplicationSummary{
			Succeeded: removed, Failed: failed,
			CopiesRemoved: removed,
		}, nil).AnyTimes()

	return ops.NewReplication(ops.ReplicationDeps{
		Replicator: repl,
		OverRep:    over,
		Backlog:    storetest.NewMockMetadataStore(ctrl),
		Config:     config.NewLive(&config.Config{Replication: config.ReplicationConfig{Factor: 2, BatchSize: 10}}),
	})
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestAdminActions_SurfaceObjectsTheCycleCouldNotFinish asserts the dashboard's
// status payload carries the objects each cycle left behind alongside the count
// it completed. The dashboard polls only this endpoint, so a pass that half
// worked has to say so here or it renders as a clean run.
func TestAdminActions_SurfaceObjectsTheCycleCouldNotFinish(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		resultKey string
		wantCount float64
	}{
		{name: "replicate", resultKey: "copies_created", wantCount: 2},
		{name: "clean-excess", resultKey: "copies_removed", wantCount: 5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Handler{log: slog.Default(), d: Deps{Ops: &ops.Services{Replication: partialReplication(t, 2, 5, 3)}}}

			w := httptest.NewRecorder()
			serveRoute(h, w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/"+tc.name, nil))
			if w.Code != http.StatusAccepted {
				t.Fatalf("trigger status = %d, want 202; body=%s", w.Code, w.Body.String())
			}
			assertRan(t, waitForResult(t, h, "/api/"+tc.name))

			statusW := httptest.NewRecorder()
			serveRoute(h, statusW, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/"+tc.name+"/status", nil))
			result, _ := decodeBody(t, statusW)["result"].(map[string]any)
			if result[tc.resultKey] != tc.wantCount {
				t.Errorf("%s = %v, want %v", tc.resultKey, result[tc.resultKey], tc.wantCount)
			}
			if result["failed"] != float64(3) {
				t.Errorf("failed = %v, want 3", result["failed"])
			}
		})
	}
}

// TestAdminActions_RouteIntoAdmin asserts that each trigger runs its admin
// pass through the real goroutine path and the poll reports the subsystem
// being off the way the admin API does: a skip with its reason, or for the
// bulk rewrites, which refuse when their layer is unavailable, a failure
// naming it.
func TestAdminActions_RouteIntoAdmin(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		opName string
		want   string
		key    string
	}{
		{"replicate", adminstream.OutcomeSkipped, "reason"},
		{"scrub", adminstream.OutcomeSkipped, "reason"},
		{"backfill-checksums", adminstream.OutcomeSkipped, "reason"},
		{"encrypt-existing", adminstream.OutcomeFailed, "error"},
		{"compress-existing", adminstream.OutcomeFailed, "error"},
		{"decompress-existing", adminstream.OutcomeFailed, "error"},
	} {
		t.Run(tc.opName, func(t *testing.T) {
			t.Parallel()
			h := newActionsHandler(t)

			triggerW := httptest.NewRecorder()
			serveRoute(h, triggerW, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/"+tc.opName, nil))
			if triggerW.Code != http.StatusAccepted {
				t.Fatalf("trigger status = %d, want %d (body=%s)",
					triggerW.Code, http.StatusAccepted, triggerW.Body.String())
			}
			waitForResult(t, h, "/api/"+tc.opName)

			statusW := httptest.NewRecorder()
			serveRoute(h, statusW, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/"+tc.opName+"/status", nil))
			if body := decodeBody(t, statusW); body["status"] != tc.want || body[tc.key] == nil {
				t.Errorf("status body = %v, want %s with a %s", body, tc.want, tc.key)
			}
		})
	}
}

// TestHandleAPICompressExisting_ReportsCounts asserts the compression wrappers
// surface a completed pass once a codec is configured, rather than only the
// skipped reason the default fixture produces. Both directions are covered
// because each names its own result key, and a mismatch there shows as a
// dashboard that reports nothing.
func TestHandleAPICompressExisting_ReportsCounts(t *testing.T) {
	t.Parallel()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(store)
	codec, err := compression.NewCodecForLevel(config.DefaultCompressionLevel, compression.MinChunkSize)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(codec.Close)

	for _, opName := range []string{"compress-existing", "decompress-existing"} {
		t.Run(opName, func(t *testing.T) {
			t.Parallel()
			h := &Handler{log: slog.Default(), d: Deps{
				Ops: &ops.Services{Compression: ops.NewCompression(&ops.CompressionDeps{
					Codec:  codec,
					Config: config.CompressionConfig{Enabled: true, Level: "default", MinRatio: 0.95},
					Store:  store,
					Mover:  opstest.NewMockMover(gomock.NewController(t)),
				})},
			}}

			w := httptest.NewRecorder()
			serveRoute(h, w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/"+opName, nil))
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
			}

			assertRan(t, waitForResult(t, h, "/api/"+opName))
		})
	}
}
