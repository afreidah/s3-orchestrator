// -------------------------------------------------------------------------------
// Ops - Operation Tests
//
// Author: Alex Freidah
//
// Covers the three answers every operation can give: a completed run with its
// counts, a declined run carrying a reason, and a rejected request. The
// declined paths matter most - both operator surfaces render them as a status
// rather than an error, so a subsystem that is merely turned off must not look
// like a failure.
// -------------------------------------------------------------------------------

package ops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// testBucket is the virtual bucket every key in these tests lives under.
const testBucket = "bucket"

// fakeBackend is a minimal in-memory ObjectBackend for the bulk-rewrite happy
// path. GetObject returns a fixed payload and PutObject records the call so a
// test can assert the rewrite reached the upload step.
type fakeBackend struct {
	payload []byte
	puts    atomic.Int64
	gets    atomic.Int64
	lastPut []byte // bytes the most recent upload carried
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// GetObject returns the fixed payload.
func (f *fakeBackend) GetObject(_ context.Context, _, _ string) (*s3be.GetObjectResult, error) {
	f.gets.Add(1)
	return &s3be.GetObjectResult{
		Body:        io.NopCloser(bytes.NewReader(f.payload)),
		Size:        int64(len(f.payload)),
		ContentType: "application/octet-stream",
	}, nil
}

// PutObject drains the body so the upload-side reader hits EOF, and counts the
// call.
func (f *fakeBackend) PutObject(_ context.Context, _ string, body io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, body); err != nil {
		return "", err
	}
	f.lastPut = buf.Bytes()
	f.puts.Add(1)
	return "etag", nil
}

// HeadObject reports the fixed payload's length.
func (f *fakeBackend) HeadObject(_ context.Context, _ string) (*s3be.HeadObjectResult, error) {
	return &s3be.HeadObjectResult{Size: int64(len(f.payload))}, nil
}

// DeleteObject is a no-op.
func (f *fakeBackend) DeleteObject(_ context.Context, _ string) error { return nil }

func (f *fakeBackend) ListObjects(context.Context, string, func([]s3be.ListedObject) error) error {
	return nil
}

// emptyEncAdmin is a minimal RewriteStore stub: every listing is empty and
// every mutator is a no-op, so a pass runs to completion with nothing to do.
type emptyEncAdmin struct{}

// ListCopiesByForm lists no locations, whatever the filter.
func (emptyEncAdmin) ListCopiesByForm(context.Context, *core.FormFilter, int, core.Cursor) ([]core.ObjectLocation, error) {
	return nil, nil
}

// CountCopiesByForm counts no locations.
func (emptyEncAdmin) CountCopiesByForm(context.Context, core.StoredFlags, core.StoredFlags) (int64, error) {
	return 0, nil
}

// UpdateCopyMetadata is a no-op.
func (emptyEncAdmin) UpdateCopyMetadata(_ context.Context, _ *core.ObjectLocation, _ *core.CopyPatch) error {
	return nil
}

// rowEncAdmin serves exactly one plaintext location, so the bulk-rewrite loop
// processes a single object and then terminates.
type rowEncAdmin struct {
	emptyEncAdmin
	row    core.ObjectLocation
	served atomic.Bool
}

// ListCopiesByForm serves the single row once to a listing of plaintext
// copies, and nothing to any other listing.
func (r *rowEncAdmin) ListCopiesByForm(_ context.Context, f *core.FormFilter, _ int, _ core.Cursor) ([]core.ObjectLocation, error) {
	if !f.Without.Has(core.StoredEncrypted) || r.served.Swap(true) {
		return nil, nil
	}
	return []core.ObjectLocation{r.row}, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// testServices builds the operations layer over a real manager and workers
// backed by the union store mock, with replication at factor 1, integrity off
// and no encryptor - the conditions every declined path checks. Backends maps
// each name to a fake so a rewrite can run end to end.
func testServices(t *testing.T, backends map[string]s3be.ObjectBackend, enc *encryption.Encryptor, encStore RewriteStore) *Services {
	t.Helper()
	return testServicesWith(t, backends, enc, encStore, &fakeMover{})
}

// testServicesWith is testServices over the given mover, for the rewrite tests
// that assert on what was placed. The mover moves between the same backends the
// fleet is built over.
func testServicesWith(t *testing.T, backends map[string]s3be.ObjectBackend, enc *encryption.Encryptor, encStore RewriteStore, mover *fakeMover) *Services {
	t.Helper()
	mover.backends = backends
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(mock)

	order := make([]string, 0, len(backends))
	for name := range backends {
		order = append(order, name)
	}
	st := proxytest.New(t, mock, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{
			Backends:        backends,
			Order:           order,
			RoutingStrategy: config.RoutingPack,
			Metrics:         mock,
		}),
	})
	workers := proxytest.BuildWorkers(st, mock)
	workers.Replicator.SetConfig(&config.ReplicationConfig{Factor: 1})
	workers.OverReplicationCleaner.SetConfig(&config.ReplicationConfig{Factor: 1})

	var store RewriteStore = mock
	if encStore != nil {
		store = encStore
	}
	return New(&Deps{
		Objects:      st.Objects,
		Store:        mock,
		Encryptor:    enc,
		EncStore:     store,
		Runtime:      st.Runtime,
		Mover:        mover,
		IntegrityCfg: st.IntegrityCfg,
		Replicator:   workers.Replicator,
		OverRep:      workers.OverReplicationCleaner,
		Rebalancer:   workers.Rebalancer,
		Scrubber:     workers.Scrubber,
		Unhashed:     mock,
		Locker:       mock,
		Declared:     declaredBuckets(testBucket),
		Cfg:          &config.Config{Buckets: []config.BucketConfig{{Name: testBucket}}},
	})
}

// enableIntegrity turns verification on for the config behind svc, so the
// integrity operations get past their disabled guard.
func enableIntegrity(t *testing.T, svc *Services) {
	t.Helper()
	cfg, ok := svc.Integrity.integrityCfg.(*syncutil.AtomicConfig[config.IntegrityConfig])
	if !ok {
		t.Fatalf("integrityCfg is %T, want the shared atomic config", svc.Integrity.integrityCfg)
	}
	cfg.Store(&config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50})
}

// assertSkipped fails unless err is a skip carrying a reason.
func assertSkipped(t *testing.T, err error) {
	t.Helper()
	var skip *SkipError
	if !errors.As(err, &skip) {
		t.Fatalf("err = %v, want a *SkipError", err)
	}
	if skip.Reason == "" {
		t.Error("skip reason is empty; want an explanation the caller can render")
	}
}

// newObjects builds an object service over a mocked object API, for the tests
// that assert validation and counting rather than real byte movement.
func newObjects(t *testing.T) (*Objects, *opstest.MockObjectAPI) {
	t.Helper()
	api := opstest.NewMockObjectAPI(gomock.NewController(t))
	return NewObjects(ObjectsDeps{
		Objects: api,
		Store:   storetest.NewMockMetadataStore(gomock.NewController(t)),
		Buckets: declaredBuckets(testBucket),
	}), api
}

// testEncryptor builds a real encryptor over a static all-zero key, for the
// paths that must get past the encryption-disabled guard.
func testEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	provider, err := encryption.NewStaticKeyProvider(make([]byte, config.KeySize))
	if err != nil {
		t.Fatalf("NewStaticKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestReplicate_SkippedAtFactorOne asserts a replication cycle declines when
// the running factor leaves nothing to copy.
func TestReplicate_SkippedAtFactorOne(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)

	res, err := svc.Replication.Replicate(context.Background(), nil)
	assertSkipped(t, err)
	if res.CopiesCreated != 0 {
		t.Errorf("CopiesCreated = %d, want 0", res.CopiesCreated)
	}
}

// TestReplicate_EmptyStore asserts a cycle runs and reports zero copies when
// the factor allows replication but nothing is under-replicated.
func TestReplicate_EmptyStore(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)
	svc.Replication.replicator.(*worker.Replicator).SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 10})

	res, err := svc.Replication.Replicate(context.Background(), nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if res.CopiesCreated != 0 {
		t.Errorf("CopiesCreated = %d, want 0", res.CopiesCreated)
	}
}

// rebalanceOver builds a rebalance service over one stubbed worker, recording
// the config each cycle ran with.
func rebalanceOver(t *testing.T, workerCfg *config.RebalanceConfig, ran *config.RebalanceConfig) *Rebalance {
	t.Helper()
	m := opstest.NewMockRebalancerOps(gomock.NewController(t))
	m.EXPECT().Config().Return(workerCfg).AnyTimes()
	m.EXPECT().Rebalance(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, cfg config.RebalanceConfig, _ progress.Observer) (worker.RebalanceSummary, error) {
			*ran = cfg
			return worker.RebalanceSummary{}, nil
		}).Times(1)

	runtime := opstest.NewMockRuntimeOps(gomock.NewController(t))
	runtime.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(nil).AnyTimes()

	return NewRebalance(RebalanceDeps{
		Rebalancer: m,
		Runtime:    runtime,
		Config:     NewConfigStore(&config.Config{}),
	})
}

// TestRebalance_AppliesDefaults asserts an operator who asks for a cycle gets
// one even when rebalancing was never configured.
func TestRebalance_AppliesDefaults(t *testing.T) {
	t.Parallel()
	var ran config.RebalanceConfig
	svc := rebalanceOver(t, nil, &ran)

	if _, err := svc.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Strategy != defaultRebalanceStrategy || ran.BatchSize != defaultRebalanceBatchSize ||
		ran.Threshold != defaultRebalanceThreshold || ran.Concurrency != defaultRebalanceConcurrency {
		t.Errorf("ran with %+v, want the spread defaults", ran)
	}
}

// TestRebalance_PreservesConfiguredValues asserts only the unset fields are
// defaulted, so a configured strategy is honoured verbatim.
func TestRebalance_PreservesConfiguredValues(t *testing.T) {
	t.Parallel()
	var ran config.RebalanceConfig
	svc := rebalanceOver(t, &config.RebalanceConfig{
		Strategy: "pack", BatchSize: 50, Threshold: 0.2, Concurrency: 8,
	}, &ran)

	if _, err := svc.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Strategy != "pack" || ran.BatchSize != 50 || ran.Threshold != 0.2 || ran.Concurrency != 8 {
		t.Errorf("ran with %+v, want the configured pack values", ran)
	}
}

// TestRebalance_NotWired asserts a deployment with no worker pool reports the
// rebalancer as unavailable rather than panicking.
func TestRebalance_NotWired(t *testing.T) {
	t.Parallel()
	runtime := opstest.NewMockRuntimeOps(gomock.NewController(t))
	svc := NewRebalance(RebalanceDeps{Runtime: runtime, Config: NewConfigStore(&config.Config{})})

	_, err := svc.Run(context.Background(), nil)
	assertSkipped(t, err)
}

// TestRebalance_QuotaMetricsFailureStillReports asserts a failed metrics
// refresh does not undo a cycle that already moved objects.
func TestRebalance_QuotaMetricsFailureStillReports(t *testing.T) {
	t.Parallel()
	m := opstest.NewMockRebalancerOps(gomock.NewController(t))
	m.EXPECT().Config().Return(nil).AnyTimes()
	m.EXPECT().Rebalance(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(worker.RebalanceSummary{Succeeded: 2}, nil).Times(1)
	runtime := opstest.NewMockRuntimeOps(gomock.NewController(t))
	runtime.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(errors.New("metrics down")).Times(1)

	svc := NewRebalance(RebalanceDeps{Rebalancer: m, Runtime: runtime, Config: NewConfigStore(&config.Config{})})

	res, err := svc.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Succeeded != 2 {
		t.Errorf("moved = %d, want 2", res.Succeeded)
	}
}

// TestScrub_SkippedWhenIntegrityDisabled asserts a scrub declines rather than
// reporting a pass that verified nothing.
func TestScrub_SkippedWhenIntegrityDisabled(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)

	res, err := svc.Integrity.Scrub(context.Background(), 0, "", nil)
	assertSkipped(t, err)
	if res.Attempted != 0 || res.Failed != 0 {
		t.Errorf("Checked=%d Failed=%d, want both 0", res.Attempted, res.Failed)
	}
}

// TestScrub_EmptyStore asserts an enabled scrub over an empty store completes
// with zero counts.
func TestScrub_EmptyStore(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)
	enableIntegrity(t, svc)

	res, err := svc.Integrity.Scrub(context.Background(), 0, "", nil)
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if res.Attempted != 0 || res.Failed != 0 {
		t.Errorf("Checked=%d Failed=%d, want both 0", res.Attempted, res.Failed)
	}
}

// TestScrub_SkippedWhenLockHeldElsewhere asserts an operator-triggered pass
// declines instead of reading the fleet a second time when the scheduled sweep
// or another instance already holds the lock. Without this a second operator
// run pays full egress for copies that are being verified right now.
func TestScrub_SkippedWhenLockHeldElsewhere(t *testing.T) {
	t.Parallel()
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().Scrub(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	icfg := opstest.NewMockIntegrityConfigLoader(gomock.NewController(t))
	icfg.EXPECT().Load().
		Return(&config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}).AnyTimes()

	locker := opstest.NewMockAdvisoryLocker(gomock.NewController(t))
	locker.EXPECT().WithAdvisoryLock(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(false, nil)

	svc := NewIntegrity(IntegrityDeps{Scrubber: scrubber, Store: newBacklog(0), IntegrityCfg: icfg, Locker: locker})

	res, err := svc.Scrub(context.Background(), 0, "", nil)
	if !errors.Is(err, ErrScrubInProgress) {
		t.Fatalf("Scrub error = %v, want ErrScrubInProgress", err)
	}
	assertSkipped(t, err)
	if res.Attempted != 0 {
		t.Errorf("Checked = %d, want 0 when the pass never ran", res.Attempted)
	}
}

// TestScrub_LockErrorSurfaces asserts a lock that fails outright is reported as
// an error rather than as a skip: an operator retrying a declined pass is fine,
// but a database that cannot answer is not the same answer.
func TestScrub_LockErrorSurfaces(t *testing.T) {
	t.Parallel()
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().Scrub(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	icfg := opstest.NewMockIntegrityConfigLoader(gomock.NewController(t))
	icfg.EXPECT().Load().
		Return(&config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}).AnyTimes()

	wantErr := errors.New("lock unavailable")
	locker := opstest.NewMockAdvisoryLocker(gomock.NewController(t))
	locker.EXPECT().WithAdvisoryLock(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(false, wantErr)

	svc := NewIntegrity(IntegrityDeps{Scrubber: scrubber, Store: newBacklog(0), IntegrityCfg: icfg, Locker: locker})

	if _, err := svc.Scrub(context.Background(), 0, "", nil); !errors.Is(err, wantErr) {
		t.Fatalf("Scrub error = %v, want %v", err, wantErr)
	}
}

// TestScrub_RunsUnderTheLock asserts the pass happens inside the locked
// section, so the lock actually covers the backend reads rather than being
// taken and released around nothing.
func TestScrub_RunsUnderTheLock(t *testing.T) {
	t.Parallel()
	var locked bool
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().Scrub(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, int, string, progress.Observer) batch.Summary {
			if !locked {
				t.Error("scrub ran outside the advisory lock")
			}
			return batch.Summary{Attempted: 3}
		})

	icfg := opstest.NewMockIntegrityConfigLoader(gomock.NewController(t))
	icfg.EXPECT().Load().
		Return(&config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}).AnyTimes()

	locker := opstest.NewMockAdvisoryLocker(gomock.NewController(t))
	locker.EXPECT().WithAdvisoryLock(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(ctx context.Context, _ int64, fn func(context.Context) error) (bool, error) {
			locked = true
			err := fn(ctx)
			locked = false
			return true, err
		})

	svc := NewIntegrity(IntegrityDeps{Scrubber: scrubber, Store: newBacklog(0), IntegrityCfg: icfg, Locker: locker})

	res, err := svc.Scrub(context.Background(), 0, "", nil)
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if res.Attempted != 3 {
		t.Errorf("Checked = %d, want 3", res.Attempted)
	}
}

// TestVerifyKey_SkippedWhenIntegrityDisabled asserts a per-key verification
// declines when verification is off, which the transports answer with a
// conflict rather than a not-found.
func TestVerifyKey_SkippedWhenIntegrityDisabled(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)

	_, err := svc.Integrity.VerifyKey(context.Background(), testBucket+"/file.txt")
	assertSkipped(t, err)
}

// TestVerifyKey_RejectsEmptyKey asserts the request is rejected before any
// store lookup.
func TestVerifyKey_RejectsEmptyKey(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)

	if _, err := svc.Integrity.VerifyKey(context.Background(), ""); !errors.Is(err, ErrKeyRequired) {
		t.Errorf("err = %v, want ErrKeyRequired", err)
	}
}

// TestBackfillChecksums_SkippedWhenIntegrityDisabled asserts the backfill
// declines with a reason rather than reporting an empty pass.
func TestBackfillChecksums_SkippedWhenIntegrityDisabled(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)

	res, err := svc.Integrity.BackfillChecksums(context.Background(), 0, 0, 0, "", nil)
	assertSkipped(t, err)
	if res.Succeeded != 0 {
		t.Errorf("Processed = %d, want 0", res.Succeeded)
	}
}

// TestBackfillChecksums_EmptyStore asserts an enabled backfill with no backlog
// reports a drained run.
func TestBackfillChecksums_EmptyStore(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)
	enableIntegrity(t, svc)

	res, err := svc.Integrity.BackfillChecksums(context.Background(), 0, 0, 0, "", nil)
	if err != nil {
		t.Fatalf("BackfillChecksums: %v", err)
	}
	if res.Succeeded != 0 {
		t.Errorf("Processed = %d, want 0", res.Succeeded)
	}
	if !res.Done {
		t.Error("Done = false, want true for an empty backlog")
	}
}

// TestEncryptExisting_SkippedWithoutEncryptor asserts the pass declines when
// the process was started without encryption.
func TestEncryptExisting_SkippedWithoutEncryptor(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)

	res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0, "")
	assertSkipped(t, err)
	if res.Succeeded != 0 || res.Failed != 0 || res.Total != 0 {
		t.Errorf("counts = (%d, %d, %d), want all 0", res.Succeeded, res.Failed, res.Total)
	}
}

// TestEncryptExisting_EmptyStore asserts the pagination loop terminates on an
// empty first page and reports zero counts.
func TestEncryptExisting_EmptyStore(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, testEncryptor(t), emptyEncAdmin{})

	res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("EncryptExisting: %v", err)
	}
	if res.Total != 0 || res.Succeeded != 0 || res.Failed != 0 {
		t.Errorf("counts = (%d, %d, %d), want all 0", res.Succeeded, res.Failed, res.Total)
	}
}

// TestEncryptExisting_OneRow asserts the whole rewrite runs for a single
// object: list, download, encrypt, re-upload, then place the encrypted copy
// in the source's stead. The placed bytes decrypt back to the object.
func TestEncryptExisting_OneRow(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t)
	fake := &fakeBackend{payload: []byte("hello world")}
	encStore := &rowEncAdmin{row: core.ObjectLocation{
		ObjectKey:   testBucket + "/file.txt",
		BackendName: "backend-a",
		SizeBytes:   int64(len(fake.payload)),
		ContentHash: "hash",
	}}
	mover := &fakeMover{}
	svc := testServicesWith(t, map[string]s3be.ObjectBackend{"backend-a": fake}, enc, encStore, mover)

	res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("EncryptExisting: %v", err)
	}
	if res.Total != 1 || res.Succeeded != 1 || res.Failed != 0 {
		t.Errorf("counts = (%d, %d, %d), want (1, 0, 1)", res.Succeeded, res.Failed, res.Total)
	}
	if fake.puts.Load() != 1 {
		t.Errorf("PutObject calls = %d, want 1", fake.puts.Load())
	}
	placed := mover.placements()
	if len(placed) != 1 {
		t.Fatalf("placements = %d, want 1", len(placed))
	}
	form := placed[0].form
	if form.Form()&core.StoredLayers != core.StoredEncrypted || form.PlaintextSize != int64(len(fake.payload)) || form.ContentHash != "hash" {
		t.Errorf("placed form = %+v, want encrypted over the payload carrying the hash", form)
	}
	plain, _, err := enc.DecryptStored(context.Background(), bytes.NewReader(placed[0].body),
		form.EncryptionKey, form.KeyID, form.PlaintextSize, nil)
	if err != nil {
		t.Fatalf("DecryptStored: %v", err)
	}
	if got, _ := io.ReadAll(plain); !bytes.Equal(got, fake.payload) {
		t.Errorf("decrypted %q, want %q", got, fake.payload)
	}
}

// TestEncryptExisting_KeepsTheEncoding checks the forward pass wraps a
// compressed copy's encoding as it is, so the copy ends up compressed and
// encrypted without being decoded.
func TestEncryptExisting_KeepsTheEncoding(t *testing.T) {
	t.Parallel()
	encoded := []byte("pretend zstd frame")
	fake := &fakeBackend{payload: encoded}
	encStore := &rowEncAdmin{row: core.ObjectLocation{
		ObjectKey: testBucket + "/file.txt", BackendName: "backend-a", SizeBytes: int64(len(encoded)),
		CompressionAlgorithm: "zstd", CompressionLevel: "default", CompressionFormatVersion: 1, LogicalSize: 4096,
	}}
	mover := &fakeMover{}
	svc := testServicesWith(t, map[string]s3be.ObjectBackend{"backend-a": fake}, testEncryptor(t), encStore, mover)

	if res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0, ""); err != nil || res.Succeeded != 1 {
		t.Fatalf("EncryptExisting = %+v, %v; want one succeeded", res, err)
	}
	placed := mover.placements()
	if len(placed) != 1 {
		t.Fatalf("placements = %d, want 1", len(placed))
	}
	form := placed[0].form
	if form.Form()&core.StoredLayers != core.StoredLayers {
		t.Errorf("placed form %v, want compressed and encrypted", form.Form())
	}
	if form.LogicalSize != 4096 || form.PlaintextSize != int64(len(encoded)) {
		t.Errorf("sizes logical=%d plaintext=%d, want 4096 and the %d byte encoding",
			form.LogicalSize, form.PlaintextSize, len(encoded))
	}
}

// decryptRowStore serves exactly one encrypted location, so the reverse
// rewrite processes a single object and then terminates.
type decryptRowStore struct {
	emptyEncAdmin
	row    core.ObjectLocation
	served atomic.Bool
}

// ListCopiesByForm serves the single row once to a listing of encrypted
// copies, and nothing to any other listing.
func (d *decryptRowStore) ListCopiesByForm(_ context.Context, f *core.FormFilter, _ int, _ core.Cursor) ([]core.ObjectLocation, error) {
	if !f.With.Has(core.StoredEncrypted) || d.served.Swap(true) {
		return nil, nil
	}
	return []core.ObjectLocation{d.row}, nil
}

// sealed encrypts payload with enc and returns the stored key material and
// ciphertext, so a test can start from an object that really is encrypted
// rather than from a hand-built row.
func sealed(t *testing.T, enc *encryption.Encryptor, payload []byte) (keyData []byte, keyID string, ciphertext []byte) {
	t.Helper()
	res, err := enc.Encrypt(context.Background(), bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext, err = io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	return encryption.PackKeyData(res.BaseNonce, res.WrappedDEK), res.KeyID, ciphertext
}

// TestDecryptExisting_OneRow asserts the reverse rewrite runs end to end: the
// ciphertext is read back as plaintext, re-uploaded, and the row is cleared.
func TestDecryptExisting_OneRow(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t)
	payload := []byte("hello world")
	keyData, keyID, ciphertext := sealed(t, enc, payload)

	fake := &fakeBackend{payload: ciphertext}
	store := &decryptRowStore{row: core.ObjectLocation{
		ObjectKey:     testBucket + "/file.txt",
		BackendName:   "backend-a",
		SizeBytes:     int64(len(ciphertext)),
		Encrypted:     true,
		PlaintextSize: int64(len(payload)),
		EncryptionKey: keyData,
		KeyID:         keyID,
	}}
	mover := &fakeMover{}
	svc := testServicesWith(t, map[string]s3be.ObjectBackend{"backend-a": fake}, enc, store, mover)

	res, err := svc.Encryption.DecryptExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("DecryptExisting: %v", err)
	}
	if res.Total != 1 || res.Succeeded != 1 || res.Failed != 0 {
		t.Errorf("res = %+v, want total=1 succeeded=1 failed=0", res)
	}
	if fake.puts.Load() != 1 {
		t.Errorf("PutObject calls = %d, want 1", fake.puts.Load())
	}
	placed := mover.placements()
	if len(placed) != 1 {
		t.Fatalf("placements = %d, want 1", len(placed))
	}
	if got := placed[0]; got.form.Form() != 0 || got.form.EncryptionKey != nil || got.size != int64(len(payload)) {
		t.Errorf("placed %+v at %d bytes, want plaintext of %d bytes with no envelope", got.form, got.size, len(payload))
	}
	if !bytes.Equal(placed[0].body, payload) {
		t.Errorf("placed %q, want %q", placed[0].body, payload)
	}
}

// rotationFilter is the listing a rotation away from keyID pages through: the
// encrypted copies wrapped under that key, on every backend.
func rotationFilter(keyID string) *core.FormFilter {
	return &core.FormFilter{With: core.StoredEncrypted, KeyID: keyID}
}

// TestRotateKey_RewrapsEveryLocation asserts a rotation re-wraps the DEK and
// records the new key material, leaving the object bytes untouched.
func TestRotateKey_RewrapsEveryLocation(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t)
	keyData, keyID, _ := sealed(t, enc, []byte("hello world"))

	var updated bool
	store := opstest.NewMockRewriteStore(gomock.NewController(t))
	first := store.EXPECT().ListCopiesByForm(gomock.Any(), rotationFilter(keyID), rotateBatchSize, core.Cursor{}).
		Return([]core.ObjectLocation{
			{ObjectKey: testBucket + "/file.txt", BackendName: "b1", EncryptionKey: keyData, KeyID: keyID},
		}, nil).Times(1)
	store.EXPECT().ListCopiesByForm(gomock.Any(), rotationFilter(keyID), gomock.Any(), gomock.Any()).
		Return(nil, nil).After(first).AnyTimes()
	store.EXPECT().UpdateCopyMetadata(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, loc *core.ObjectLocation, p *core.CopyPatch) error {
			if loc.ObjectKey != testBucket+"/file.txt" || loc.BackendName != "b1" {
				t.Errorf("patched copy = %s on %s, want %s on b1", loc.ObjectKey, loc.BackendName, testBucket+"/file.txt")
			}
			updated = len(p.EncryptionKey) > 0 && p.KeyID != ""
			return nil
		}).Times(1)

	svc := NewEncryption(EncryptionDeps{
		Encryptor: enc,
		Store:     store,
		Mover:     &fakeMover{},
	})

	res, err := svc.RotateKey(context.Background(), keyID, false)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if res.Rotated != 1 || res.Failed != 0 || res.Total != 1 {
		t.Errorf("res = %+v, want rotated=1 failed=0 total=1", res)
	}
	if !updated {
		t.Error("UpdateCopyMetadata received no new key material")
	}
}

// TestRotateKey_ReplacedCopyIsSkipped asserts a copy a newer write replaced
// mid-pass counts toward Total but as neither rotated nor failed.
func TestRotateKey_ReplacedCopyIsSkipped(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t)
	keyData, keyID, _ := sealed(t, enc, []byte("hello world"))

	store := opstest.NewMockRewriteStore(gomock.NewController(t))
	first := store.EXPECT().ListCopiesByForm(gomock.Any(), rotationFilter(keyID), rotateBatchSize, core.Cursor{}).
		Return([]core.ObjectLocation{
			{ObjectKey: testBucket + "/file.txt", BackendName: "b1", EncryptionKey: keyData, KeyID: keyID},
		}, nil).Times(1)
	store.EXPECT().ListCopiesByForm(gomock.Any(), rotationFilter(keyID), gomock.Any(), gomock.Any()).
		Return(nil, nil).After(first).AnyTimes()
	store.EXPECT().UpdateCopyMetadata(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(core.ErrCopyChanged).Times(1)

	svc := NewEncryption(EncryptionDeps{
		Encryptor: enc,
		Store:     store,
		Mover:     &fakeMover{},
	})

	res, err := svc.RotateKey(context.Background(), keyID, false)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if res.Rotated != 0 || res.Failed != 0 || res.Total != 1 {
		t.Errorf("res = %+v, want rotated=0 failed=0 total=1", res)
	}
}

// TestEncryptExisting_FailureModesAreCountedPerObject asserts each way one
// object can fail leaves the pass running and nothing placed: the read, the
// re-upload, and the placement each fail on their own.
func TestEncryptExisting_FailureModesAreCountedPerObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		method   backendtest.Method
		placeErr error
	}{
		{name: "download fails", method: backendtest.MethodGet},
		{name: "re-upload fails", method: backendtest.MethodPut},
		{name: "placement fails", placeErr: errors.New("ledger unavailable")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key := testBucket + "/file.txt"
			be := seededBackend(t, key)
			if tc.placeErr == nil {
				be.SetErr(tc.method, backendtest.ErrSimulatedBackendFailure)
			}
			store := &rowEncAdmin{row: core.ObjectLocation{ObjectKey: key, BackendName: "backend-a", SizeBytes: 11}}
			mover := &fakeMover{placeErr: tc.placeErr}
			svc := testServicesWith(t, map[string]s3be.ObjectBackend{"backend-a": be}, testEncryptor(t), store, mover)

			res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0, "")
			if err != nil {
				t.Fatalf("EncryptExisting: %v", err)
			}
			if res.Total != 1 || res.Failed != 1 || res.Succeeded != 0 {
				t.Errorf("res = %+v, want total=1 failed=1 succeeded=0", res)
			}
			if n := len(mover.placements()); n != 0 {
				t.Errorf("placements = %d, want none for an object that was never rewritten", n)
			}
		})
	}
}

// seededBackend returns an in-memory backend holding an 11-byte object at key,
// with failure injection available for the test to arm.
func seededBackend(t *testing.T, key string) *backendtest.FailableBackend {
	t.Helper()
	be := backendtest.New(backendtest.NewInMemory())
	if _, err := be.PutObject(context.Background(), key, bytes.NewReader([]byte("hello world")), 11, "", nil); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	return be
}

// TestRotateKey_SkippedWithoutEncryptor asserts rotation declines rather than
// reporting an empty run when encryption is off.
func TestRotateKey_SkippedWithoutEncryptor(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)

	_, err := svc.Encryption.RotateKey(context.Background(), "old-key", false)
	assertSkipped(t, err)
}

// TestRotateKey_RequiresExactlyOneScope asserts a rotation names either one
// old key or every stale key, never both and never neither.
func TestRotateKey_RequiresExactlyOneScope(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, testEncryptor(t), emptyEncAdmin{})

	for _, tc := range []struct {
		name     string
		oldKeyID string
		allStale bool
	}{
		{"neither", "", false},
		{"both", "old-key", true},
	} {
		if _, err := svc.Encryption.RotateKey(context.Background(), tc.oldKeyID, tc.allStale); !errors.Is(err, ErrKeyIDRequired) {
			t.Errorf("%s: err = %v, want ErrKeyIDRequired", tc.name, err)
		}
	}
}

// TestRotateKey_AllStaleSkipsCurrentKey asserts an all-stale rotation pages
// through every encrypted copy except those already under the current key.
func TestRotateKey_AllStaleSkipsCurrentKey(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t)
	want := &core.FormFilter{With: core.StoredEncrypted, ExceptKeyID: enc.Provider().KeyID()}

	store := opstest.NewMockRewriteStore(gomock.NewController(t))
	store.EXPECT().ListCopiesByForm(gomock.Any(), want, rotateBatchSize, core.Cursor{}).Return(nil, nil).Times(1)

	svc := NewEncryption(EncryptionDeps{
		Encryptor: enc,
		Store:     store,
		Mover:     &fakeMover{},
	})
	res, err := svc.RotateKey(context.Background(), "", true)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if res.Total != 0 {
		t.Errorf("res = %+v, want an empty run", res)
	}
}

// TestObjects_RejectsKeyOutsideBucket asserts every keyed operation refuses a
// key that names no configured bucket, before any backend is contacted.
func TestObjects_RejectsKeyOutsideBucket(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)
	ctx := context.Background()

	if _, err := svc.Objects.Get(ctx, "nosuchbucket/file.txt"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Get err = %v, want ErrInvalidKey", err)
	}
	if err := svc.Objects.Delete(ctx, "nosuchbucket/file.txt"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Delete err = %v, want ErrInvalidKey", err)
	}
	if _, err := svc.Objects.Put(ctx, "nosuchbucket/file.txt", bytes.NewReader(nil), 0, ""); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Put err = %v, want ErrInvalidKey", err)
	}
}

// TestObjects_RejectsEmptyKeyAndPrefix asserts the empty request is answered
// as a rejection rather than as a fleet-wide operation.
func TestObjects_RejectsEmptyKeyAndPrefix(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)
	ctx := context.Background()

	if err := svc.Objects.Delete(ctx, ""); !errors.Is(err, ErrKeyRequired) {
		t.Errorf("Delete err = %v, want ErrKeyRequired", err)
	}
	if _, err := svc.Objects.DeletePrefix(ctx, "", nil); !errors.Is(err, ErrPrefixRequired) {
		t.Errorf("DeletePrefix err = %v, want ErrPrefixRequired", err)
	}
}

// TestObjectsGet_NotFound asserts a missing object is reported as ErrNotFound
// rather than as a backend failure, so a transport can answer 404.
func TestObjectsGet_NotFound(t *testing.T) {
	t.Parallel()
	objects, api := newObjects(t)
	api.EXPECT().
		GetObject(gomock.Any(), testBucket+"/ghost", "").
		Return(nil, core.ErrObjectNotFound).
		Times(1)

	if _, err := objects.Get(context.Background(), testBucket+"/ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// integrityOver builds an integrity service over one stubbed scrubber with
// verification enabled.
func integrityOver(t *testing.T, scrubber ScrubberOps) *Integrity {
	t.Helper()
	return integrityWithBacklog(t, scrubber, newBacklog(0))
}

// integrityWithBacklog builds an Integrity over scrubber whose backfill lists
// from store.
func integrityWithBacklog(t *testing.T, scrubber ScrubberOps, store UnhashedLister) *Integrity {
	t.Helper()
	icfg := opstest.NewMockIntegrityConfigLoader(gomock.NewController(t))
	icfg.EXPECT().Load().
		Return(&config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}).AnyTimes()
	return NewIntegrity(IntegrityDeps{Scrubber: scrubber, Store: store, IntegrityCfg: icfg, Locker: grantingLocker(t)})
}

// grantingLocker is an advisory locker that always takes the lock and runs the
// work, which is what every test that is not about lock contention wants.
func grantingLocker(t *testing.T) *opstest.MockAdvisoryLocker {
	t.Helper()
	locker := opstest.NewMockAdvisoryLocker(gomock.NewController(t))
	locker.EXPECT().WithAdvisoryLock(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(ctx context.Context, _ int64, fn func(context.Context) error) (bool, error) {
			return true, fn(ctx)
		}).AnyTimes()
	return locker
}

// TestVerifyKey_ReportsEveryCopy asserts a per-key verification answers with a
// verdict per copy, which is the whole reason to verify one object on demand.
func TestVerifyKey_ReportsEveryCopy(t *testing.T) {
	t.Parallel()
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().ScrubKey(gomock.Any(), testBucket+"/file.txt").
		Return([]worker.CopyVerification{
			{Backend: "b1", Outcome: worker.CopyVerified},
			{Backend: "b2", Outcome: worker.CopyMismatch},
		}, nil).Times(1)

	copies, err := integrityOver(t, scrubber).VerifyKey(context.Background(), testBucket+"/file.txt")
	if err != nil {
		t.Fatalf("VerifyKey: %v", err)
	}
	if len(copies) != 2 {
		t.Errorf("copies = %d, want one verdict per copy", len(copies))
	}
}

// TestVerifyKey_NoCopiesIsNotFound keeps "no copies recorded" from reading as a
// successful verification of nothing.
func TestVerifyKey_NoCopiesIsNotFound(t *testing.T) {
	t.Parallel()
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().ScrubKey(gomock.Any(), gomock.Any()).Return(nil, nil).Times(1)

	_, err := integrityOver(t, scrubber).VerifyKey(context.Background(), testBucket+"/ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestBackfillChecksums_StopsAtObjectCap asserts the cap bounds one request so
// it fits a client timeout, and reports that the backlog is not drained.
func TestBackfillChecksums_StopsAtObjectCap(t *testing.T) {
	t.Parallel()
	b := newBacklog(40)

	res, err := integrityWithBacklog(t, backlogHasher(t, b, nil), b).BackfillChecksums(context.Background(), 10, 25, 0, "", nil)
	if err != nil {
		t.Fatalf("BackfillChecksums: %v", err)
	}
	// The cap is checked between pages, so the page that crossed it completes.
	if res.Succeeded != 30 || len(b.keys) != 10 {
		t.Errorf("Processed = %d with %d left, want 30 processed and 10 left", res.Succeeded, len(b.keys))
	}
	if res.Done {
		t.Error("Done = true, want false when the cap stopped the run short of the backlog")
	}
}

// TestBackfillChecksums_ReportsUnreadable asserts the copies each pass skipped
// as undecodable are summed across passes into the run's result.
func TestBackfillChecksums_ReportsUnreadable(t *testing.T) {
	t.Parallel()
	// Unreadable copies stay unhashed, so the cursor has to step past them
	// rather than list them again.
	b := newBacklog(13)
	unreadable := func(key string) bool { return key < "k-005" }

	res, err := integrityWithBacklog(t, backlogHasher(t, b, unreadable), b).BackfillChecksums(context.Background(), 10, 0, 0, "", nil)
	if err != nil {
		t.Fatalf("BackfillChecksums: %v", err)
	}
	if res.Skipped != 5 || res.Succeeded != 8 || !res.Done {
		t.Errorf("res = %+v, want 5 unreadable, 8 processed and the backlog drained", res)
	}
}

// TestPurgeUnreadable_DefaultsTheBatchAndReportsTheTally asserts a zero batch
// size asks the scrubber for the default, and the result carries its tally.
func TestPurgeUnreadable_DefaultsTheBatchAndReportsTheTally(t *testing.T) {
	t.Parallel()
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().PurgeUnreadable(gomock.Any(), 100, gomock.Any()).Return(batch.Summary{Succeeded: 103, Failed: 2})

	res := integrityOver(t, scrubber).PurgeUnreadable(context.Background(), 0, nil)
	if res.Succeeded != 103 || res.Failed != 2 {
		t.Errorf("res = %+v, want 103 purged and 2 failed", res)
	}
}

// TestListUnreadable_DefaultsTheLimit asserts a zero limit asks the scrubber
// for the default page and passes the total through.
func TestListUnreadable_DefaultsTheLimit(t *testing.T) {
	t.Parallel()
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().ListUnreadable(gomock.Any(), 100).
		Return([]core.ObjectLocation{{ObjectKey: "bucket/a", BackendName: "b1"}}, int64(7), nil)

	res, err := integrityOver(t, scrubber).ListUnreadable(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListUnreadable: %v", err)
	}
	if res.Total != 7 || len(res.Copies) != 1 {
		t.Errorf("res = %+v, want 1 copy of 7", res)
	}
}

// TestEncryptExisting_CountsUnreachableBackend asserts one location whose
// backend is gone fails on its own rather than ending the pass.
func TestEncryptExisting_CountsUnreachableBackend(t *testing.T) {
	t.Parallel()
	encStore := &rowEncAdmin{row: core.ObjectLocation{
		ObjectKey:   testBucket + "/file.txt",
		BackendName: "missing-backend",
		SizeBytes:   11,
	}}
	mover := &fakeMover{}
	svc := testServicesWith(t, map[string]s3be.ObjectBackend{}, testEncryptor(t), encStore, mover)

	res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("EncryptExisting: %v", err)
	}
	if res.Total != 1 || res.Failed != 1 || res.Succeeded != 0 {
		t.Errorf("res = %+v, want total=1 failed=1 succeeded=0", res)
	}
	if n := len(mover.placements()); n != 0 {
		t.Errorf("placements = %d, want none for a backend that is gone", n)
	}
}

// TestObjectsGet_ReturnsBody asserts a stored object comes back with the body
// and size the caller needs to save it.
func TestObjectsGet_ReturnsBody(t *testing.T) {
	t.Parallel()
	objects, api := newObjects(t)
	payload := []byte("hello world")
	api.EXPECT().GetObject(gomock.Any(), testBucket+"/file.txt", "").
		Return(&object.GetResult{
			GetObjectResult: &s3be.GetObjectResult{
				Body: io.NopCloser(bytes.NewReader(payload)),
				Size: int64(len(payload)),
			},
		}, nil).Times(1)

	res, err := objects.Get(context.Background(), testBucket+"/file.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("body = %q, want %q", got, payload)
	}
}

// TestObjectsPut_ReturnsETag asserts a stored object reports the ETag the
// backend recorded it under.
func TestObjectsPut_ReturnsETag(t *testing.T) {
	t.Parallel()
	objects, api := newObjects(t)
	api.EXPECT().PutObject(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, req *object.PutObjectRequest) (string, error) {
			if req.Key != testBucket+"/file.txt" || req.Size != 5 || req.ContentType != "text/plain" {
				t.Errorf("unexpected put request: %+v", req)
			}
			return "etag-1", nil
		}).Times(1)

	etag, err := objects.Put(context.Background(), testBucket+"/file.txt", bytes.NewReader([]byte("hello")), 5, "text/plain")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if etag != "etag-1" {
		t.Errorf("etag = %q, want etag-1", etag)
	}
}

// TestObjectsDelete_RemovesKey asserts a delete reaches the object manager, so
// every copy is removed rather than only the ledger row.
func TestObjectsDelete_RemovesKey(t *testing.T) {
	t.Parallel()
	objects, api := newObjects(t)
	api.EXPECT().DeleteObject(gomock.Any(), testBucket+"/file.txt").Return(nil).Times(1)

	if err := objects.Delete(context.Background(), testBucket+"/file.txt"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

// TestObjectsList_GroupsOnADelimiter asserts a delimiter listing collapses
// directories, which is what a browser expects.
func TestObjectsList_GroupsOnADelimiter(t *testing.T) {
	t.Parallel()
	store := storetest.NewMockObjectStore(gomock.NewController(t))
	store.EXPECT().
		ListObjects(gomock.Any(), "", "/", "", defaultListMaxKeys).
		Return(&core.ListObjectsResult{CommonPrefixes: []string{testBucket + "/"}}, nil).
		Times(1)
	objects := NewObjects(ObjectsDeps{
		Objects: opstest.NewMockObjectAPI(gomock.NewController(t)),
		Store:   store,
		Buckets: declaredBuckets(),
	})

	page, err := objects.List(context.Background(), "", "/", "", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.CommonPrefixes) != 1 {
		t.Errorf("common prefixes = %v, want the bucket root", page.CommonPrefixes)
	}
}

// TestObjectsList_FlatWithoutADelimiter asserts an empty delimiter lists every
// key under the prefix, which is what a caller counting or sweeping a subtree
// needs. The grouping default belongs to the transport, not here.
func TestObjectsList_FlatWithoutADelimiter(t *testing.T) {
	t.Parallel()
	store := storetest.NewMockObjectStore(gomock.NewController(t))
	store.EXPECT().
		ListObjects(gomock.Any(), testBucket+"/dir/", "", "", defaultListMaxKeys).
		Return(&core.ListObjectsResult{
			Objects:     []core.ObjectLocation{{ObjectKey: testBucket + "/dir/a"}, {ObjectKey: testBucket + "/dir/b"}},
			IsTruncated: true,
		}, nil).
		Times(1)
	objects := NewObjects(ObjectsDeps{
		Objects: opstest.NewMockObjectAPI(gomock.NewController(t)),
		Store:   store,
		Buckets: declaredBuckets(),
	})

	page, err := objects.List(context.Background(), testBucket+"/dir/", "", "", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != 2 || len(page.CommonPrefixes) != 0 {
		t.Errorf("page = %+v, want two flat keys and no grouping", page)
	}
	if !page.IsTruncated {
		t.Error("truncation should carry through, so a caller knows the page is a floor")
	}
}

// TestObjectsLocations_ReportsEveryCopy asserts the placement query answers
// with each backend holding the key.
func TestObjectsLocations_ReportsEveryCopy(t *testing.T) {
	t.Parallel()
	store := storetest.NewMockObjectStore(gomock.NewController(t))
	store.EXPECT().GetAllObjectLocations(gomock.Any(), testBucket+"/file.txt").
		Return([]core.ObjectLocation{{BackendName: "b1"}, {BackendName: "b2"}}, nil).Times(1)
	objects := NewObjects(ObjectsDeps{
		Objects: opstest.NewMockObjectAPI(gomock.NewController(t)),
		Store:   store,
		Buckets: declaredBuckets(),
	})

	locs, err := objects.Locations(context.Background(), testBucket+"/file.txt")
	if err != nil {
		t.Fatalf("Locations: %v", err)
	}
	if len(locs) != 2 {
		t.Errorf("locations = %d, want 2", len(locs))
	}
	if _, err := objects.Locations(context.Background(), ""); !errors.Is(err, ErrKeyRequired) {
		t.Errorf("empty key err = %v, want ErrKeyRequired", err)
	}
}

// TestCountSurplus_ReportsBacklog asserts the surplus count is answered at the
// running factor, and declines when replication is meaningless.
func TestCountSurplus_ReportsBacklog(t *testing.T) {
	t.Parallel()
	over := opstest.NewMockOverReplicationOps(gomock.NewController(t))
	over.EXPECT().Config().Return(&config.ReplicationConfig{Factor: 3}).AnyTimes()
	over.EXPECT().CountPending(gomock.Any(), 3).Return(int64(7), nil).Times(1)
	svc := replicationOver(t, opstest.NewMockReplicatorOps(gomock.NewController(t)), over)

	res, err := svc.CountSurplus(context.Background())
	if err != nil {
		t.Fatalf("CountSurplus: %v", err)
	}
	if res.Factor != 3 || res.Pending != 7 {
		t.Errorf("res = %+v, want factor=3 pending=7", res)
	}
}

// TestReplicate_ReportsObjectsItCouldNotCopy asserts the objects a cycle left
// under-replicated reach the caller. Without the count, a pass that created
// nothing because every object failed is indistinguishable from one that had
// nothing to do.
func TestReplicate_ReportsObjectsItCouldNotCopy(t *testing.T) {
	t.Parallel()
	repl := opstest.NewMockReplicatorOps(gomock.NewController(t))
	repl.EXPECT().Config().Return(&config.ReplicationConfig{Factor: 2}).AnyTimes()
	repl.EXPECT().Replicate(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(worker.ReplicationSummary{
			Succeeded: 1, Failed: 2,
			CopiesCreated: 3,
		}, nil).Times(1)
	svc := replicationOver(t, repl, opstest.NewMockOverReplicationOps(gomock.NewController(t)))

	res, err := svc.Replicate(context.Background(), nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if res.CopiesCreated != 3 {
		t.Errorf("CopiesCreated = %d, want 3", res.CopiesCreated)
	}
	if res.Failed != 2 {
		t.Errorf("Failed = %d, want 2", res.Failed)
	}
}

// TestCleanExcess_ReportsObjectsItCouldNotClean asserts the objects whose
// surplus survived the cycle reach the caller.
func TestCleanExcess_ReportsObjectsItCouldNotClean(t *testing.T) {
	t.Parallel()
	over := opstest.NewMockOverReplicationOps(gomock.NewController(t))
	over.EXPECT().Config().Return(&config.ReplicationConfig{Factor: 2}).AnyTimes()
	over.EXPECT().Clean(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(worker.OverReplicationSummary{
			Failed:        5,
			CopiesRemoved: 0,
		}, nil).Times(1)
	svc := replicationOver(t, opstest.NewMockReplicatorOps(gomock.NewController(t)), over)

	res, err := svc.CleanExcess(context.Background(), 0, nil)
	if err != nil {
		t.Fatalf("CleanExcess: %v", err)
	}
	if res.CopiesRemoved != 0 {
		t.Errorf("CopiesRemoved = %d, want 0", res.CopiesRemoved)
	}
	if res.Failed != 5 {
		t.Errorf("Failed = %d, want 5", res.Failed)
	}
}

// TestCleanExcess_CapsBatchSize asserts a caller cannot schedule an unbounded
// pass by asking for one.
func TestCleanExcess_CapsBatchSize(t *testing.T) {
	t.Parallel()
	var ran config.ReplicationConfig
	over := opstest.NewMockOverReplicationOps(gomock.NewController(t))
	over.EXPECT().Config().Return(&config.ReplicationConfig{Factor: 2}).AnyTimes()
	over.EXPECT().Clean(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, cfg config.ReplicationConfig, _ progress.Observer) (worker.OverReplicationSummary, error) {
			ran = cfg
			return worker.OverReplicationSummary{
				Succeeded:     4,
				CopiesRemoved: 4,
			}, nil
		}).Times(1)
	svc := replicationOver(t, opstest.NewMockReplicatorOps(gomock.NewController(t)), over)

	res, err := svc.CleanExcess(context.Background(), maxCleanBatchSize*10, nil)
	if err != nil {
		t.Fatalf("CleanExcess: %v", err)
	}
	if res.CopiesRemoved != 4 {
		t.Errorf("CopiesRemoved = %d, want 4", res.CopiesRemoved)
	}
	if ran.BatchSize != maxCleanBatchSize {
		t.Errorf("ran with batch size %d, want the cap %d", ran.BatchSize, maxCleanBatchSize)
	}
}

// replicationOver builds a replication service over the two stubbed workers.
func replicationOver(t *testing.T, repl ReplicatorOps, over OverReplicationOps) *Replication {
	t.Helper()
	runtime := opstest.NewMockRuntimeOps(gomock.NewController(t))
	runtime.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(nil).AnyTimes()
	return NewReplication(ReplicationDeps{
		Replicator: repl,
		OverRep:    over,
		Runtime:    runtime,
		Config:     NewConfigStore(&config.Config{}),
	})
}

// TestDecryptExisting_EmptyStore asserts the reverse pass shares the driver
// and terminates on an empty first page.
func TestDecryptExisting_EmptyStore(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, testEncryptor(t), emptyEncAdmin{})

	res, err := svc.Encryption.DecryptExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("DecryptExisting: %v", err)
	}
	if res.Total != 0 {
		t.Errorf("Total = %d, want 0", res.Total)
	}
}

// TestRotateKey_CountsFailuresPerLocation asserts one unusable row fails on
// its own rather than ending the pass.
func TestRotateKey_CountsFailuresPerLocation(t *testing.T) {
	t.Parallel()
	store := opstest.NewMockRewriteStore(gomock.NewController(t))
	first := store.EXPECT().ListCopiesByForm(gomock.Any(), rotationFilter("old"), rotateBatchSize, core.Cursor{}).
		Return([]core.ObjectLocation{
			{ObjectKey: "k1", BackendName: "b1", EncryptionKey: []byte{0x01}, KeyID: "old"},
		}, nil).Times(1)
	store.EXPECT().ListCopiesByForm(gomock.Any(), rotationFilter("old"), gomock.Any(), gomock.Any()).
		Return(nil, nil).After(first).AnyTimes()

	svc := NewEncryption(EncryptionDeps{
		Encryptor: testEncryptor(t),
		Store:     store,
		Mover:     &fakeMover{},
	})

	res, err := svc.RotateKey(context.Background(), "old", false)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if res.Total != 1 || res.Failed != 1 || res.Rotated != 0 {
		t.Errorf("res = %+v, want total=1 failed=1 rotated=0", res)
	}
}

// TestDeclaredBuckets_UpdateReachesOperations asserts republishing the declared
// set changes what a later run accepts, which is why the object operations hold
// the live set rather than a snapshot. A bucket created through the
// provisioning API becomes addressable here without a restart.
func TestDeclaredBuckets_UpdateReachesOperations(t *testing.T) {
	t.Parallel()
	declared := declaredBuckets()
	api := opstest.NewMockObjectAPI(gomock.NewController(t))
	api.EXPECT().DeleteObject(gomock.Any(), "later/file.txt").Return(nil).AnyTimes()
	objects := NewObjects(ObjectsDeps{
		Objects: api,
		Store:   storetest.NewMockObjectStore(gomock.NewController(t)),
		Buckets: declared,
	})

	if err := objects.Delete(context.Background(), "later/file.txt"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("err = %v, want ErrInvalidKey before the bucket exists", err)
	}

	declared.Set([]provisioning.Bucket{{Name: "later", Source: provisioning.SourceStore}})

	if err := objects.Delete(context.Background(), "later/file.txt"); errors.Is(err, ErrInvalidKey) {
		t.Error("key still rejected after the bucket was declared")
	}
}

// TestSkipError_Message asserts a skip renders its reason, so a log line or a
// generic error handler still says why nothing happened.
func TestSkipError_Message(t *testing.T) {
	t.Parallel()
	if got := Skip("nothing to do").Error(); got != "skipped: nothing to do" {
		t.Errorf("Error() = %q, want the reason", got)
	}
}

// errBackend is what a backend reports when the operation reached it and
// failed there, as opposed to being rejected before it started.
var errBackend = errors.New("backend unavailable")

// TestObjects_PropagateBackendFailures asserts a failure at the backend is
// returned as-is rather than folded into one of the rejection sentinels, so a
// transport answers 500 rather than 400.
func TestObjects_PropagateBackendFailures(t *testing.T) {
	t.Parallel()
	key := testBucket + "/file.txt"

	t.Run("get", func(t *testing.T) {
		t.Parallel()
		objects, api := newObjects(t)
		api.EXPECT().GetObject(gomock.Any(), key, "").Return(nil, errBackend).Times(1)
		if _, err := objects.Get(context.Background(), key); !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the backend failure", err)
		}
	})

	t.Run("put", func(t *testing.T) {
		t.Parallel()
		objects, api := newObjects(t)
		api.EXPECT().PutObject(gomock.Any(), gomock.Any()).
			Return("", errBackend).Times(1)
		if _, err := objects.Put(context.Background(), key, bytes.NewReader(nil), 0, ""); !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the backend failure", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		t.Parallel()
		objects, api := newObjects(t)
		api.EXPECT().DeleteObject(gomock.Any(), key).Return(errBackend).Times(1)
		if err := objects.Delete(context.Background(), key); !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the backend failure", err)
		}
	})

	t.Run("delete prefix listing", func(t *testing.T) {
		t.Parallel()
		objects, api := newObjects(t)
		api.EXPECT().ListObjects(gomock.Any(), testBucket+"/dir/", "", "", deletePrefixPageSize).
			Return(nil, errBackend).Times(1)
		res, err := objects.DeletePrefix(context.Background(), testBucket+"/dir/", nil)
		if !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the listing failure", err)
		}
		if res.Deleted != 0 {
			t.Errorf("Deleted = %d, want 0 when the listing never returned keys", res.Deleted)
		}
	})
}

// TestObjects_RejectsEveryKeyWithoutConfig asserts an operations layer that
// has no configuration yet refuses keys rather than accepting anything.
func TestObjects_RejectsEveryKeyWithoutConfig(t *testing.T) {
	t.Parallel()
	objects := NewObjects(ObjectsDeps{
		Objects: opstest.NewMockObjectAPI(gomock.NewController(t)),
		Store:   storetest.NewMockObjectStore(gomock.NewController(t)),
		Buckets: declaredBuckets(),
	})

	if err := objects.Delete(context.Background(), testBucket+"/file.txt"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("err = %v, want ErrInvalidKey", err)
	}
}

// TestWorkerFailures_Propagate asserts a worker that fails mid-cycle surfaces
// the error rather than a zero-count success.
func TestWorkerFailures_Propagate(t *testing.T) {
	t.Parallel()

	t.Run("replicate", func(t *testing.T) {
		t.Parallel()
		repl := opstest.NewMockReplicatorOps(gomock.NewController(t))
		repl.EXPECT().Config().Return(&config.ReplicationConfig{Factor: 2}).AnyTimes()
		repl.EXPECT().Replicate(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(worker.ReplicationSummary{}, errBackend).Times(1)
		svc := replicationOver(t, repl, opstest.NewMockOverReplicationOps(gomock.NewController(t)))

		if _, err := svc.Replicate(context.Background(), nil); !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the worker failure", err)
		}
	})

	t.Run("clean excess", func(t *testing.T) {
		t.Parallel()
		over := opstest.NewMockOverReplicationOps(gomock.NewController(t))
		over.EXPECT().Config().Return(&config.ReplicationConfig{Factor: 2}).AnyTimes()
		over.EXPECT().Clean(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(worker.OverReplicationSummary{}, errBackend).Times(1)
		svc := replicationOver(t, opstest.NewMockReplicatorOps(gomock.NewController(t)), over)

		if _, err := svc.CleanExcess(context.Background(), 0, nil); !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the worker failure", err)
		}
	})

	t.Run("count surplus", func(t *testing.T) {
		t.Parallel()
		over := opstest.NewMockOverReplicationOps(gomock.NewController(t))
		over.EXPECT().Config().Return(&config.ReplicationConfig{Factor: 2}).AnyTimes()
		over.EXPECT().CountPending(gomock.Any(), gomock.Any()).Return(int64(0), errBackend).Times(1)
		svc := replicationOver(t, opstest.NewMockReplicatorOps(gomock.NewController(t)), over)

		if _, err := svc.CountSurplus(context.Background()); !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the store failure", err)
		}
	})

	t.Run("rebalance", func(t *testing.T) {
		t.Parallel()
		m := opstest.NewMockRebalancerOps(gomock.NewController(t))
		m.EXPECT().Config().Return(nil).AnyTimes()
		m.EXPECT().Rebalance(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(worker.RebalanceSummary{}, errBackend).Times(1)
		svc := NewRebalance(RebalanceDeps{
			Rebalancer: m,
			Runtime:    opstest.NewMockRuntimeOps(gomock.NewController(t)),
			Config:     NewConfigStore(&config.Config{}),
		})

		if _, err := svc.Run(context.Background(), nil); !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the worker failure", err)
		}
	})
}

// TestBackfillChecksums_StopsOnCancelledContext asserts an operator who
// disconnects mid-run stops the pass rather than leaving it draining the
// backlog, and that the inter-batch pause honours the same cancellation.
func TestBackfillChecksums_StopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().HashCopies(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, locs []core.ObjectLocation, observer progress.Observer) batch.Summary {
			progress.Track(observer, "key", func() string { return progress.StatusOK })
			cancel()
			return batch.Summary{Attempted: len(locs), Succeeded: len(locs)}
		}).Times(1)

	res, err := integrityWithBacklog(t, scrubber, newBacklog(20)).BackfillChecksums(ctx, 10, 0, time.Millisecond, "", nil)
	if err != nil {
		t.Fatalf("BackfillChecksums: %v", err)
	}
	if res.Done {
		t.Error("Done = true, want false for a run stopped by cancellation")
	}
}

// TestBackfillChecksums_PausesBetweenPasses asserts the inter-batch pause runs
// between passes, which is what rate-limits backend reads on a large backlog.
func TestBackfillChecksums_PausesBetweenPasses(t *testing.T) {
	t.Parallel()
	b := newBacklog(15)

	start := time.Now()
	res, err := integrityWithBacklog(t, backlogHasher(t, b, nil), b).BackfillChecksums(context.Background(), 10, 0, 20*time.Millisecond, "", nil)
	if err != nil {
		t.Fatalf("BackfillChecksums: %v", err)
	}
	if !res.Done {
		t.Error("Done = false, want true once the backlog drained")
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("elapsed = %v, want at least the configured pause between passes", elapsed)
	}
}

// TestDeletePrefix_CountsAndReports asserts a prefix delete pages the listing,
// counts what it removed, separates the failures, and reports one step per key
// so a streaming caller can render progress.
func TestDeletePrefix_CountsAndReports(t *testing.T) {
	t.Parallel()
	objects, api := newObjects(t)
	prefix := testBucket + "/dir/"
	keys := []string{prefix + "a", prefix + "b", prefix + "c"}

	api.EXPECT().
		ListObjects(gomock.Any(), prefix, "", "", deletePrefixPageSize).
		Return(&core.ListObjectsResult{Objects: []core.ObjectLocation{
			{ObjectKey: keys[0]}, {ObjectKey: keys[1]}, {ObjectKey: keys[2]},
		}}, nil).
		Times(1)
	api.EXPECT().
		DeleteObjects(gomock.Any(), keys).
		Return([]object.DeleteObjectResult{
			{Key: keys[0]},
			{Key: keys[1], Err: errors.New("backend refused")},
			{Key: keys[2]},
		}).
		Times(1)

	var steps []progress.Step
	res, err := objects.DeletePrefix(context.Background(), prefix, func(s progress.Step) {
		steps = append(steps, s)
	})
	if err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if res.Deleted != 2 || res.Failed != 1 || res.Total != 3 {
		t.Errorf("result = %+v, want deleted=2 failed=1 total=3", res)
	}
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want one per key", len(steps))
	}
	if steps[1].Status != "failed" || steps[1].Label != keys[1] {
		t.Errorf("step for the failed key = %+v, want label=%s status=failed", steps[1], keys[1])
	}
}
