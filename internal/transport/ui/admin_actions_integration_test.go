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
			Metrics:         mock,
		}),
	})
	workers := proxytest.BuildWorkers(st, mock)
	workers.Replicator.SetConfig(&config.ReplicationConfig{Factor: 1})
	workers.OverReplicationCleaner.SetConfig(&config.ReplicationConfig{Factor: 1})
	for _, opt := range opts {
		opt(st, workers)
	}

	return testOps(st, workers, mock)
}

// testOps assembles the operations layer over a stack and its workers, the
// same wiring the composition root performs.
func testOps(st *proxytest.Stack, workers *proxytest.Workers, store storetest.MetadataStore) *ops.Services {
	return ops.New(&ops.Deps{
		Objects:      st.Objects,
		Store:        store,
		EncStore:     store,
		CompStore:    store,
		Locker:       store,
		Runtime:      st.Runtime,
		Mover:        st.Coord,
		IntegrityCfg: st.IntegrityCfg,
		Replicator:   workers.Replicator,
		OverRep:      workers.OverReplicationCleaner,
		Rebalancer:   workers.Rebalancer,
		Scrubber:     workers.Scrubber,
		Unhashed:     store,
		Declared:     declaredFrom(&config.Config{Buckets: []config.BucketConfig{{Name: "test-bucket"}}}),
		Cfg:          &config.Config{Buckets: []config.BucketConfig{{Name: "test-bucket"}}},
	})
}

// newActionsHandler builds a UI handler whose operations are the ones under
// test. Every operation resolves to the skipped branch unless opts say
// otherwise.
func newActionsHandler(t testing.TB, opts ...func(*proxytest.Stack, *proxytest.Workers)) *Handler {
	t.Helper()
	svc := newOpsForTest(t, opts...)
	return &Handler{log: slog.Default(), d: Deps{
		Objects:     svc.Objects,
		Integrity:   svc.Integrity,
		Replication: svc.Replication,
		Rebalance:   svc.Rebalance,
		Encryption:  svc.Encryption,
		Compression: svc.Compression,
	}}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestHandleAPIReplicate_HappyPathReturnsCount asserts that with replication
// enabled (factor > 1) the status endpoint reports the CopiesCreated count
// rather than a skipped reason.
func TestHandleAPIReplicate_HappyPathReturnsCount(t *testing.T) {
	t.Parallel()
	h := newActionsHandler(t, func(_ *proxytest.Stack, workers *proxytest.Workers) {
		workers.Replicator.SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 10})
	})

	triggerReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/replicate", nil)
	triggerW := httptest.NewRecorder()
	serveRoute(h, triggerW, triggerReq)
	if triggerW.Code != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want %d", triggerW.Code, http.StatusAccepted)
	}

	res := waitForResult(t, h, "replicate")
	if res.Error != "" || res.Skipped != "" {
		t.Errorf("result = %+v, want no error and no Skipped reason", res)
	}
	if res.Counts.Count != 0 {
		t.Errorf("Count = %d, want 0 (empty store)", res.Counts.Count)
	}

	statusReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/replicate/status", nil)
	statusW := httptest.NewRecorder()
	serveRoute(h, statusW, statusReq)
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
		Return(&object.GetResult{
			GetObjectResult: &backend.GetObjectResult{
				Body: io.NopCloser(bytes.NewReader(payload)),
				Size: int64(len(payload)),
			},
		}, nil).Times(1)

	h := &Handler{log: slog.Default(), d: Deps{
		Objects: ops.NewObjects(ops.ObjectsDeps{
			Objects: api,
			Store:   storetest.NewMockObjectStore(gomock.NewController(t)),
			Buckets: declaredFrom(&config.Config{Buckets: []config.BucketConfig{{Name: "test-bucket"}}}),
		}),
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
		Encryption: ops.NewEncryption(ops.EncryptionDeps{
			Encryptor: enc,
			Store:     store,
			Mover:     opstest.NewMockMover(gomock.NewController(t)),
		}),
	}}

	w := httptest.NewRecorder()
	serveRoute(h, w, httptest.NewRequestWithContext(context.Background(),
		http.MethodPost, "/api/encrypt-existing", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	res := waitForResult(t, h, "encrypt-existing")
	if res.Error != "" || res.Skipped != "" {
		t.Errorf("result = %+v, want a completed pass with no skip reason", res)
	}
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
				st.IntegrityCfg.Store(&config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 10})
			})

			w := httptest.NewRecorder()
			serveRoute(h, w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/"+opName, nil))
			if w.Code != http.StatusAccepted {
				t.Fatalf("trigger status = %d, want 202", w.Code)
			}

			res := waitForResult(t, h, opName)
			if res.Error != "" || res.Skipped != "" {
				t.Errorf("result = %+v, want a completed pass with no skip reason", res)
			}
		})
	}
}

// TestHandleAPICleanExcess_ReportsRemovedCount asserts the surplus cleanup
// reports what it removed once the factor makes the pass meaningful.
func TestHandleAPICleanExcess_ReportsRemovedCount(t *testing.T) {
	t.Parallel()
	h := newActionsHandler(t, func(_ *proxytest.Stack, workers *proxytest.Workers) {
		workers.OverReplicationCleaner.SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 10})
	})

	w := httptest.NewRecorder()
	serveRoute(h, w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/clean-excess", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	res := waitForResult(t, h, opCleanExcess)
	if res.Error != "" || res.Skipped != "" {
		t.Errorf("result = %+v, want a completed pass with no skip reason", res)
	}
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
	cfg := &config.ReplicationConfig{Factor: 2, BatchSize: 10}

	repl := opstest.NewMockReplicatorOps(ctrl)
	repl.EXPECT().Config().Return(cfg).AnyTimes()
	repl.EXPECT().Replicate(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(worker.ReplicationSummary{
			Succeeded: created, Failed: failed,
			CopiesCreated: created,
		}, nil).AnyTimes()

	over := opstest.NewMockOverReplicationOps(ctrl)
	over.EXPECT().Config().Return(cfg).AnyTimes()
	over.EXPECT().Clean(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(worker.OverReplicationSummary{
			Succeeded: removed, Failed: failed,
			CopiesRemoved: removed,
		}, nil).AnyTimes()

	runtime := opstest.NewMockRuntimeOps(ctrl)
	runtime.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(nil).AnyTimes()

	return ops.NewReplication(ops.ReplicationDeps{
		Replicator: repl,
		OverRep:    over,
		Runtime:    runtime,
		Config:     ops.NewConfigStore(&config.Config{}),
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
		opName    string
		resultKey string
		wantCount float64
	}{
		{name: "replicate", opName: "replicate", resultKey: "copies_created", wantCount: 2},
		{name: "clean-excess", opName: opCleanExcess, resultKey: "removed", wantCount: 5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Handler{log: slog.Default(), d: Deps{Replication: partialReplication(t, 2, 5, 3)}}

			w := httptest.NewRecorder()
			serveRoute(h, w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/"+tc.name, nil))
			if w.Code != http.StatusAccepted {
				t.Fatalf("trigger status = %d, want 202; body=%s", w.Code, w.Body.String())
			}

			res := waitForResult(t, h, tc.opName)
			if res.Error != "" || res.Skipped != "" {
				t.Fatalf("result = %+v, want a completed pass with no skip reason", res)
			}

			statusW := httptest.NewRecorder()
			serveRoute(h, statusW, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/"+tc.name+"/status", nil))
			body := decodeBody(t, statusW)
			if body[tc.resultKey] != tc.wantCount {
				t.Errorf("%s = %v, want %v", tc.resultKey, body[tc.resultKey], tc.wantCount)
			}
			if body["failed"] != float64(3) {
				t.Errorf("failed = %v, want 3", body["failed"])
			}
		})
	}
}

// TestAdminActions_RouteIntoAdmin asserts that each trigger actually invokes
// its admin counterpart through the real goroutine path and surfaces the
// skipped reason on the status endpoint.
func TestAdminActions_RouteIntoAdmin(t *testing.T) {
	t.Parallel()

	for _, opName := range []string{
		"replicate", "scrub", "backfill-checksums", "encrypt-existing", "compress-existing", "decompress-existing",
	} {
		t.Run(opName, func(t *testing.T) {
			t.Parallel()
			h := newActionsHandler(t)

			triggerW := httptest.NewRecorder()
			serveRoute(h, triggerW, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/"+opName, nil))
			if triggerW.Code != http.StatusAccepted {
				t.Fatalf("trigger status = %d, want %d (body=%s)",
					triggerW.Code, http.StatusAccepted, triggerW.Body.String())
			}

			res := waitForResult(t, h, opName)
			if res.Skipped == "" {
				t.Errorf("op %q result has empty Skipped reason; want non-empty", opName)
			}

			statusW := httptest.NewRecorder()
			serveRoute(h, statusW, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/"+opName+"/status", nil))
			if body := decodeBody(t, statusW); body["status"] != "skipped" {
				t.Errorf("op %q status body = %v, want status=skipped", opName, body)
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
	codec, err := compression.NewCodec(compression.DefaultLevel, compression.MinChunkSize)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(codec.Close)

	for _, opName := range []string{"compress-existing", "decompress-existing"} {
		t.Run(opName, func(t *testing.T) {
			t.Parallel()
			h := &Handler{log: slog.Default(), d: Deps{
				Compression: ops.NewCompression(&ops.CompressionDeps{
					Codec:  codec,
					Config: config.CompressionConfig{Enabled: true, Level: "default", MinRatio: 0.95},
					Store:  store,
					Mover:  opstest.NewMockMover(gomock.NewController(t)),
				}),
			}}

			w := httptest.NewRecorder()
			serveRoute(h, w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/"+opName, nil))
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
			}

			res := waitForResult(t, h, opName)
			if res.Error != "" || res.Skipped != "" {
				t.Errorf("result = %+v, want a completed pass with no skip reason", res)
			}
		})
	}
}
