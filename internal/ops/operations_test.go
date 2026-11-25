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
	"slices"
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
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
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
	return testServicesWith(t, backends, enc, encStore, &fakeRewriter{})
}

// testServicesWith is testServices over the given rewriter, for the rewrite tests
// that assert on what was placed. The rewriter moves between the same backends the
// fleet is built over.
func testServicesWith(t *testing.T, backends map[string]s3be.ObjectBackend, enc *encryption.Encryptor, encStore RewriteStore, rewriter *fakeRewriter) *Services {
	t.Helper()
	rewriter.backends = backends
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
		}),
	})
	workers := proxytest.BuildWorkers(st, mock)
	st.Config.Update(func(c *config.Config) {
		c.Buckets = []config.BucketConfig{{Name: testBucket}}
		c.Replication.Factor = 1
	})

	var store RewriteStore = mock
	if encStore != nil {
		store = encStore
	}
	return New(&Deps{
		Objects:    st.Objects,
		Store:      mock,
		Encryptor:  enc,
		EncStore:   store,
		Rewriter:   rewriter,
		Replicator: workers.Replicator,
		OverRep:    workers.OverReplicationCleaner,
		Backlog:    mock,
		Rebalancer: workers.Rebalancer,
		Scrubber:   workers.Scrubber,
		CopyStore:  mock,
		Locker:     mock,
		Declared:   declaredBuckets(testBucket),
		Config:     st.Config,
	})
}

// enableIntegrity turns verification on in the running config behind svc, so
// the integrity operations get past their disabled guard.
func enableIntegrity(t *testing.T, svc *Services) {
	t.Helper()
	svc.Integrity.d.Config.Update(func(c *config.Config) {
		c.Integrity = config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}
	})
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
	svc.Replication.d.Config.Update(func(c *config.Config) {
		c.Replication = config.ReplicationConfig{Factor: 2, BatchSize: 10}
	})

	res, err := svc.Replication.Replicate(context.Background(), nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if res.CopiesCreated != 0 {
		t.Errorf("CopiesCreated = %d, want 0", res.CopiesCreated)
	}
}

// rebalanceOver builds a rebalance service over one stubbed worker and the
// configured rebalance section, recording the config each cycle ran with.
func rebalanceOver(t *testing.T, configured config.RebalanceConfig, ran *config.RebalanceConfig) *Rebalance {
	t.Helper()
	m := opstest.NewMockRebalancer(gomock.NewController(t))
	m.EXPECT().Rebalance(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, cfg config.RebalanceConfig, _ progress.Observer) (worker.RebalanceSummary, error) {
			*ran = cfg
			return worker.RebalanceSummary{}, nil
		}).Times(1)

	return NewRebalance(RebalanceDeps{
		Rebalancer: m,
		Locker:     testx.Locker{},
		Config:     config.NewLive(&config.Config{Rebalance: configured}),
	})
}

// encryptionOver builds an encryption service over enc and store that moves
// nothing, under a lock it always takes.
func encryptionOver(enc *encryption.Encryptor, store RewriteStore) *Encryption {
	return NewEncryption(EncryptionDeps{Encryptor: enc, Store: store, Rewriter: &fakeRewriter{}, Locker: testx.Locker{}})
}

// TestRebalance_RunsTheRunningConfig asserts a manual cycle runs with the
// running config as loaded, defaults included, so it and the scheduled
// rebalancer agree.
func TestRebalance_RunsTheRunningConfig(t *testing.T) {
	t.Parallel()
	var ran config.RebalanceConfig
	svc := rebalanceOver(t, config.RebalanceConfig{
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
	svc := NewRebalance(RebalanceDeps{Locker: testx.Locker{}, Config: config.NewLive(&config.Config{})})

	_, err := svc.Run(context.Background(), nil)
	assertSkipped(t, err)
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

// TestManualPasses_DeclineWhileTheirLockIsHeld asserts every pass that holds an
// advisory lock declines with its busy error while the lock is held, without
// reaching its worker or store, and reports a lock call that failed as that
// failure rather than as a skip.
func TestManualPasses_DeclineWhileTheirLockIsHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lockErr := errors.New("lock store down")
	cases := []struct {
		name string
		busy error
		run  func(s *Services) error
	}{
		{"scrub", ErrScrubInProgress, func(s *Services) error { return errOf(s.Integrity.Scrub(ctx, 0, "", nil)) }},
		{"backfill", ErrBackfillInProgress, func(s *Services) error { return errOf(s.Integrity.BackfillChecksums(ctx, 0, 0, 0, "", nil)) }},
		{"reconcile", ErrReconcileInProgress, func(s *Services) error { return errOf(s.Integrity.Reconcile(ctx, "", nil)) }},
		{"replicate", ErrReplicationInProgress, func(s *Services) error { return errOf(s.Replication.Replicate(ctx, nil)) }},
		{"clean excess", ErrReplicationInProgress, func(s *Services) error { return errOf(s.Replication.CleanExcess(ctx, 0, nil)) }},
		{"rebuild queue", ErrReplicationInProgress, func(s *Services) error { return errOf(s.Replication.RebuildQueue(ctx)) }},
		{"rebalance", ErrRebalanceInProgress, func(s *Services) error { return errOf(s.Rebalance.Run(ctx, nil)) }},
		{"encrypt existing", ErrRewriteInProgress, func(s *Services) error { return errOf(s.Encryption.EncryptExisting(ctx, nil, 0, "")) }},
		{"decrypt existing", ErrRewriteInProgress, func(s *Services) error { return errOf(s.Encryption.DecryptExisting(ctx, nil, 0, "")) }},
		{"rotate key", ErrRewriteInProgress, func(s *Services) error { return errOf(s.Encryption.RotateKey(ctx, nil, "old", false)) }},
		{"compress existing", ErrRewriteInProgress, func(s *Services) error { return errOf(s.Compression.CompressExisting(ctx, nil, 0, "")) }},
		{"decompress existing", ErrRewriteInProgress, func(s *Services) error { return errOf(s.Compression.DecompressExisting(ctx, nil, 0, "")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.run(servicesLockedBy(t, testx.Locker{Busy: true})); !errors.Is(err, tc.busy) {
				t.Errorf("lock held: err = %v, want %v", err, tc.busy)
			}
			if err := tc.run(servicesLockedBy(t, testx.Locker{Err: lockErr})); !errors.Is(err, lockErr) {
				t.Errorf("lock call failed: err = %v, want %v", err, lockErr)
			}
		})
	}
}

// servicesLockedBy builds every service whose passes hold an advisory lock,
// over l and over workers and stores that fail the test if a pass reaches them.
func servicesLockedBy(t *testing.T, l testx.Locker) *Services {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := opstest.NewMockRewriteStore(ctrl)
	return &Services{
		Integrity: NewIntegrity(IntegrityDeps{
			Scrubber:   opstest.NewMockScrubberOps(ctrl),
			Reconciler: opstest.NewMockReconciler(ctrl),
			Store:      opstest.NewMockCopyStore(ctrl),
			Locker:     l,
			Config:     integrityEnabled(),
		}),
		Replication: NewReplication(ReplicationDeps{
			Replicator: opstest.NewMockReplicator(ctrl),
			OverRep:    opstest.NewMockOverReplicationCleaner(ctrl),
			Backlog:    emptyBacklog,
			Locker:     l,
			Config:     config.NewLive(&config.Config{Replication: config.ReplicationConfig{Factor: 2}}),
		}),
		Rebalance:   NewRebalance(RebalanceDeps{Rebalancer: opstest.NewMockRebalancer(ctrl), Locker: l, Config: config.NewLive(&config.Config{})}),
		Encryption:  NewEncryption(EncryptionDeps{Encryptor: testEncryptor(t), Store: store, Rewriter: &fakeRewriter{}, Locker: l}),
		Compression: NewCompression(&CompressionDeps{Codec: testCodec(t), Store: store, Rewriter: &fakeRewriter{}, Locker: l}),
	}
}

// errOf returns the error of a pass, dropping its tally.
func errOf[T any](_ T, err error) error { return err }

// TestScrub_RunsUnderTheLock asserts the pass happens inside the locked
// section, so the lock actually covers the backend reads rather than being
// taken and released around nothing.
func TestScrub_RunsUnderTheLock(t *testing.T) {
	t.Parallel()
	var locked bool
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().Scrub(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, int, string, progress.Observer) (batch.Summary, error) {
			if !locked {
				t.Error("scrub ran outside the advisory lock")
			}
			return batch.Summary{Attempted: 3}, nil
		})

	locker := lockFunc(func(ctx context.Context, _ int64, fn func(context.Context) error) (bool, error) {
		locked = true
		err := fn(ctx)
		locked = false
		return true, err
	})

	d := integrityDeps(t, scrubber)
	d.Locker = locker
	svc := NewIntegrity(d)

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
	rewriter := &fakeRewriter{}
	svc := testServicesWith(t, map[string]s3be.ObjectBackend{"backend-a": fake}, enc, encStore, rewriter)

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
	placed := rewriter.placements()
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
	rewriter := &fakeRewriter{}
	svc := testServicesWith(t, map[string]s3be.ObjectBackend{"backend-a": fake}, testEncryptor(t), encStore, rewriter)

	if res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0, ""); err != nil || res.Succeeded != 1 {
		t.Fatalf("EncryptExisting = %+v, %v; want one succeeded", res, err)
	}
	placed := rewriter.placements()
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
	rewriter := &fakeRewriter{}
	svc := testServicesWith(t, map[string]s3be.ObjectBackend{"backend-a": fake}, enc, store, rewriter)

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
	placed := rewriter.placements()
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
// records the new key material, leaving the object bytes untouched, and
// reports the copy to the observer as one step.
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

	svc := encryptionOver(enc, store)

	var steps []progress.Step
	res, err := svc.RotateKey(context.Background(), func(s progress.Step) { steps = append(steps, s) }, keyID, false)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if res.Succeeded != 1 || res.Failed != 0 || res.Planned != 1 {
		t.Errorf("res = %+v, want rotated=1 failed=0 total=1", res)
	}
	if !updated {
		t.Error("UpdateCopyMetadata received no new key material")
	}
	if len(steps) != 2 || steps[1].Label != testBucket+"/file.txt" || steps[1].Status != progress.StatusOK {
		t.Errorf("steps = %+v, want a start and an ok end for %s/file.txt", steps, testBucket)
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

	svc := encryptionOver(enc, store)

	res, err := svc.RotateKey(context.Background(), nil, keyID, false)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if res.Succeeded != 0 || res.Failed != 0 || res.Skipped != 1 {
		t.Errorf("res = %+v, want rotated=0 failed=0 skipped=1", res)
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
			rewriter := &fakeRewriter{placeErr: tc.placeErr}
			svc := testServicesWith(t, map[string]s3be.ObjectBackend{"backend-a": be}, testEncryptor(t), store, rewriter)

			res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0, "")
			if err != nil {
				t.Fatalf("EncryptExisting: %v", err)
			}
			if res.Total != 1 || res.Failed != 1 || res.Succeeded != 0 {
				t.Errorf("res = %+v, want total=1 failed=1 succeeded=0", res)
			}
			if n := len(rewriter.placements()); n != 0 {
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

	_, err := svc.Encryption.RotateKey(context.Background(), nil, "old-key", false)
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
		if _, err := svc.Encryption.RotateKey(context.Background(), nil, tc.oldKeyID, tc.allStale); !errors.Is(err, ErrKeyIDRequired) {
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

	svc := encryptionOver(enc, store)
	res, err := svc.RotateKey(context.Background(), nil, "", true)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if res.Planned != 0 {
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
	if _, err := svc.Objects.Put(ctx, &object.PutObjectRequest{Key: "nosuchbucket/file.txt", Body: bytes.NewReader(nil)}); !errors.Is(err, ErrInvalidKey) {
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

// integrityDeps is an integrity service's collaborators: one stubbed scrubber,
// a copy store that expects no call, a free lock and verification enabled. A
// test replaces the one it exercises.
func integrityDeps(t *testing.T, scrubber ScrubberOps) IntegrityDeps {
	t.Helper()
	return IntegrityDeps{
		Scrubber: scrubber,
		Store:    opstest.NewMockCopyStore(gomock.NewController(t)),
		Locker:   testx.Locker{},
		Config:   integrityEnabled(),
	}
}

// integrityOver builds an integrity service over one stubbed scrubber with
// verification enabled.
func integrityOver(t *testing.T, scrubber ScrubberOps) *Integrity {
	t.Helper()
	return NewIntegrity(integrityDeps(t, scrubber))
}

// integrityListing builds an integrity service whose listings read store.
func integrityListing(t *testing.T, store CopyStore) *Integrity {
	t.Helper()
	d := integrityDeps(t, opstest.NewMockScrubberOps(gomock.NewController(t)))
	d.Store = store
	return NewIntegrity(d)
}

// integrityEnabled is a running config with verification on, so the integrity
// operations get past their disabled guard.
func integrityEnabled() *config.Live {
	return config.NewLive(&config.Config{Integrity: config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}})
}

// lockFunc is an advisory locker that runs a function the test supplies, for a
// test that observes what happens inside the lock.
type lockFunc func(ctx context.Context, lockID int64, fn func(context.Context) error) (bool, error)

// WithAdvisoryLock calls the function.
func (f lockFunc) WithAdvisoryLock(ctx context.Context, lockID int64, fn func(context.Context) error) (bool, error) {
	return f(ctx, lockID, fn)
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

// TestBackfillChecksums_DefaultsTheBatchAndPassesTheRequest asserts a zero
// batch size asks the scrubber for the default, the rest of the request
// reaches it as given, and its result comes back unchanged.
func TestBackfillChecksums_DefaultsTheBatchAndPassesTheRequest(t *testing.T) {
	t.Parallel()
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	want := worker.BackfillRequest{BatchSize: defaultBackfillBatchSize, MaxObjects: 25, Pause: time.Second, Backend: "b1"}
	scrubber.EXPECT().Backfill(gomock.Any(), want, gomock.Any()).
		Return(worker.BackfillResult{Summary: batch.Summary{Succeeded: 25}}, nil)

	res, err := integrityOver(t, scrubber).BackfillChecksums(context.Background(), 0, 25, time.Second, "b1", nil)
	if err != nil || res.Succeeded != 25 {
		t.Errorf("res, err = %+v, %v; want the scrubber's 25", res, err)
	}
}

// TestPurgeUnreadable_DefaultsTheBatchAndReportsTheTally asserts a zero batch
// size asks the scrubber for the default, and the result carries its tally.
func TestPurgeUnreadable_DefaultsTheBatchAndReportsTheTally(t *testing.T) {
	t.Parallel()
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().PurgeUnreadable(gomock.Any(), 100, gomock.Any()).Return(batch.Summary{Succeeded: 103, Failed: 2}, nil)

	res, err := integrityOver(t, scrubber).PurgeUnreadable(context.Background(), 0, nil)
	if err != nil || res.Succeeded != 103 || res.Failed != 2 {
		t.Errorf("res, err = %+v, %v; want 103 purged and 2 failed", res, err)
	}
}

// TestListUnreadable_DefaultsTheLimit asserts a zero limit asks the store for
// the default page of unreadable copies, and the total is the ledger's
// unreadable figure.
func TestListUnreadable_DefaultsTheLimit(t *testing.T) {
	t.Parallel()
	store := opstest.NewMockCopyStore(gomock.NewController(t))
	store.EXPECT().ListCopiesByForm(gomock.Any(), &core.UnreadableCopies, defaultCopyListLimit, core.Cursor{}).
		Return([]core.ObjectLocation{{ObjectKey: "bucket/a", BackendName: "b1"}}, nil)
	store.EXPECT().LedgerStats(gomock.Any()).Return(core.LedgerStats{"b1": {Unreadable: 4}, "b2": {Unreadable: 3}}, nil)

	res, err := integrityListing(t, store).ListUnreadable(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListUnreadable: %v", err)
	}
	if res.Total != 7 || len(res.Copies) != 1 {
		t.Errorf("res = %+v, want 1 copy of 7", res)
	}
}

// TestListUnhashed_NarrowsTheListingAndTheTotal asserts the backend and
// management reach the store's filter, and the total counts only the copies
// that filter selects.
func TestListUnhashed_NarrowsTheListingAndTheTotal(t *testing.T) {
	t.Parallel()
	store := opstest.NewMockCopyStore(gomock.NewController(t))
	filter := core.UnhashedCopies("b1", new(false))
	store.EXPECT().ListCopiesByForm(gomock.Any(), &filter, 5, core.Cursor{}).
		Return([]core.ObjectLocation{{ObjectKey: "bucket/stray", BackendName: "b1", Unmanaged: true}}, nil)
	store.EXPECT().LedgerStats(gomock.Any()).Return(core.LedgerStats{
		"b1": {Unhashed: 3, UnmanagedUnhashed: 4},
		"b2": {UnmanagedUnhashed: 9},
	}, nil)

	res, err := integrityListing(t, store).ListUnhashed(context.Background(), "b1", new(false), 5)
	if err != nil {
		t.Fatalf("ListUnhashed: %v", err)
	}
	if res.Total != 4 || len(res.Copies) != 1 {
		t.Errorf("res = %+v, want 1 copy of the 4 unmanaged on b1", res)
	}
}

// TestListUnhashed_ReportsStoreErrors asserts a failed page or a failed
// ledger read fails the listing rather than answering with a partial one.
func TestListUnhashed_ReportsStoreErrors(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("store unavailable")

	listing := opstest.NewMockCopyStore(gomock.NewController(t))
	listing.EXPECT().ListCopiesByForm(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, wantErr)
	if _, err := integrityListing(t, listing).ListUnhashed(context.Background(), "", nil, 0); !errors.Is(err, wantErr) {
		t.Errorf("list failure: err = %v, want %v", err, wantErr)
	}

	ledger := opstest.NewMockCopyStore(gomock.NewController(t))
	ledger.EXPECT().ListCopiesByForm(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	ledger.EXPECT().LedgerStats(gomock.Any()).Return(nil, wantErr)
	if _, err := integrityListing(t, ledger).ListUnhashed(context.Background(), "", nil, 0); !errors.Is(err, wantErr) {
		t.Errorf("ledger failure: err = %v, want %v", err, wantErr)
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
	rewriter := &fakeRewriter{}
	svc := testServicesWith(t, map[string]s3be.ObjectBackend{}, testEncryptor(t), encStore, rewriter)

	res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("EncryptExisting: %v", err)
	}
	if res.Total != 1 || res.Failed != 1 || res.Succeeded != 0 {
		t.Errorf("res = %+v, want total=1 failed=1 succeeded=0", res)
	}
	if n := len(rewriter.placements()); n != 0 {
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

	etag, err := objects.Put(context.Background(), &object.PutObjectRequest{
		Key: testBucket + "/file.txt", Body: bytes.NewReader([]byte("hello")), Size: 5, ContentType: "text/plain",
	})
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
		ListObjects(gomock.Any(), "", "/", "", listPageSize).
		Return(&core.ListObjectsResult{CommonPrefixes: []string{testBucket + "/"}}, nil).
		Times(1)
	objects := NewObjects(ObjectsDeps{
		Objects: opstest.NewMockObjectAPI(gomock.NewController(t)),
		Store:   store,
		Buckets: declaredBuckets(),
	})

	page, err := objects.List(context.Background(), "", "/", "")
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
		ListObjects(gomock.Any(), testBucket+"/dir/", "", "", listPageSize).
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

	page, err := objects.List(context.Background(), testBucket+"/dir/", "", "")
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

// browseObjects builds an Objects over store for the file browser tests.
func browseObjects(t *testing.T, store ObjectStore) *Objects {
	t.Helper()
	return NewObjects(ObjectsDeps{
		Objects: opstest.NewMockObjectAPI(gomock.NewController(t)),
		Store:   store,
		Buckets: declaredBuckets(),
	})
}

// TestObjectsBrowse_TotalsOnlyThePagesFolders asserts a page resumed past a
// folder totals only the keys from that cursor to just past its last entry,
// and lists its folders with their totals ahead of its files.
func TestObjectsBrowse_TotalsOnlyThePagesFolders(t *testing.T) {
	t.Parallel()
	prefix := testBucket + "/"
	store := storetest.NewMockObjectStore(gomock.NewController(t))
	store.EXPECT().ListObjects(gomock.Any(), prefix, "/", prefix+"a/", 2).Return(&core.ListObjectsResult{
		CommonPrefixes:        []string{prefix + "c/"},
		Objects:               []core.ObjectLocation{{ObjectKey: prefix + "d", SizeBytes: 7}},
		IsTruncated:           true,
		NextContinuationToken: prefix + "d",
	}, nil)
	store.EXPECT().DirectoryStats(gomock.Any(), prefix, core.PrefixEnd(prefix+"a/"), prefix+"d\x01").
		Return([]core.DirStat{{Name: "c/", FileCount: 4, TotalSize: 40}}, nil)
	store.EXPECT().GetObjectBackendsForKeys(gomock.Any(), []string{prefix + "d"}).
		Return(map[string][]string{prefix + "d": {"b1"}}, nil)

	page, err := browseObjects(t, store).Browse(context.Background(), prefix, prefix+"a/", 2)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(page.Entries) != 2 || !page.HasMore || page.NextCursor != prefix+"d" {
		t.Fatalf("page = %+v, want two entries resuming after %sd", page, prefix)
	}
	if dir := page.Entries[0]; dir.Name != prefix+"c/" || !dir.IsDir || dir.FileCount != 4 || dir.TotalSize != 40 {
		t.Errorf("folder = %+v, want c/ with 4 files and 40 bytes", dir)
	}
	if file := page.Entries[1]; file.Name != prefix+"d" || file.TotalSize != 7 || len(file.Backends) != 1 {
		t.Errorf("file = %+v, want d of 7 bytes on b1", file)
	}
}

// TestObjectsBrowse_FilesOnlySkipsTheTotals asserts a page holding no folder
// never asks for folder totals.
func TestObjectsBrowse_FilesOnlySkipsTheTotals(t *testing.T) {
	t.Parallel()
	prefix := testBucket + "/"
	store := storetest.NewMockObjectStore(gomock.NewController(t))
	store.EXPECT().ListObjects(gomock.Any(), prefix, "/", "", BrowsePageSize).
		Return(&core.ListObjectsResult{Objects: []core.ObjectLocation{{ObjectKey: prefix + "a"}}}, nil)
	store.EXPECT().GetObjectBackendsForKeys(gomock.Any(), []string{prefix + "a"}).Return(map[string][]string{}, nil)

	page, err := browseObjects(t, store).Browse(context.Background(), prefix, "", BrowsePageSize)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(page.Entries) != 1 || page.HasMore {
		t.Errorf("page = %+v, want the one file and nothing after it", page)
	}
}

// TestObjectsBrowse_ReadErrorsSurface asserts whichever store read fails, the
// page fails rather than listing what the other reads returned.
func TestObjectsBrowse_ReadErrorsSurface(t *testing.T) {
	t.Parallel()
	boom := errors.New("db down")
	folders := &core.ListObjectsResult{CommonPrefixes: []string{testBucket + "/a/"}}
	a := gomock.Any()
	for _, tc := range []struct {
		name  string
		stubs func(m *storetest.MockObjectStore)
	}{
		{"listing", func(m *storetest.MockObjectStore) {
			m.EXPECT().ListObjects(a, a, a, a, a).Return(nil, boom)
		}},
		{"totals", func(m *storetest.MockObjectStore) {
			m.EXPECT().ListObjects(a, a, a, a, a).Return(folders, nil)
			m.EXPECT().DirectoryStats(a, a, a, a).Return(nil, boom)
		}},
		{"backends", func(m *storetest.MockObjectStore) {
			m.EXPECT().ListObjects(a, a, a, a, a).Return(folders, nil)
			m.EXPECT().DirectoryStats(a, a, a, a).Return(nil, nil)
			m.EXPECT().GetObjectBackendsForKeys(a, a).Return(nil, boom)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := storetest.NewMockObjectStore(gomock.NewController(t))
			tc.stubs(store)
			if _, err := browseObjects(t, store).Browse(context.Background(), testBucket+"/", "", 2); !errors.Is(err, boom) {
				t.Errorf("err = %v, want the %s failure", err, tc.name)
			}
		})
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

// TestCountBacklog_ReportsBothSides asserts the backlog is the queue's two
// sides, reported with the running factor.
func TestCountBacklog_ReportsBothSides(t *testing.T) {
	t.Parallel()
	svc := replicationCounting(t, opstest.NewMockReplicator(gomock.NewController(t)),
		opstest.NewMockOverReplicationCleaner(gomock.NewController(t)),
		backlogFunc(func(context.Context) (core.ReplicationBacklog, error) {
			return core.ReplicationBacklog{Under: 3, Over: 7}, nil
		}))

	res, err := svc.CountBacklog(context.Background())
	if err != nil {
		t.Fatalf("CountBacklog: %v", err)
	}
	if res != (Backlog{Factor: 2, Under: 3, Over: 7}) {
		t.Errorf("res = %+v, want factor=2 under=3 over=7", res)
	}
}

// TestCountBacklog_DisabledReportsTheFactor asserts a disabled replication
// reports its factor with ErrReplicationDisabled and reads nothing.
func TestCountBacklog_DisabledReportsTheFactor(t *testing.T) {
	t.Parallel()
	svc := NewReplication(ReplicationDeps{
		Replicator: opstest.NewMockReplicator(gomock.NewController(t)),
		OverRep:    opstest.NewMockOverReplicationCleaner(gomock.NewController(t)),
		Backlog: backlogFunc(func(context.Context) (core.ReplicationBacklog, error) {
			t.Error("a disabled backlog read the queue")
			return core.ReplicationBacklog{}, nil
		}),
		Locker: testx.Locker{},
		Config: config.NewLive(&config.Config{Replication: config.ReplicationConfig{Factor: 1}}),
	})

	res, err := svc.CountBacklog(context.Background())
	if !errors.Is(err, ErrReplicationDisabled) || res != (Backlog{Factor: 1}) {
		t.Errorf("CountBacklog = %+v, %v; want factor 1 with ErrReplicationDisabled", res, err)
	}
}

// TestReplicate_ReportsObjectsItCouldNotCopy asserts the objects a cycle left
// under-replicated reach the caller. Without the count, a pass that created
// nothing because every object failed is indistinguishable from one that had
// nothing to do.
func TestReplicate_ReportsObjectsItCouldNotCopy(t *testing.T) {
	t.Parallel()
	repl := opstest.NewMockReplicator(gomock.NewController(t))
	repl.EXPECT().Replicate(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(worker.ReplicationSummary{
			Succeeded: 1, Failed: 2,
			CopiesCreated: 3,
		}, nil).Times(1)
	svc := replicationOver(t, repl, opstest.NewMockOverReplicationCleaner(gomock.NewController(t)))

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
	over := opstest.NewMockOverReplicationCleaner(gomock.NewController(t))
	over.EXPECT().Clean(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(worker.OverReplicationSummary{
			Failed:        5,
			CopiesRemoved: 0,
		}, nil).Times(1)
	svc := replicationOver(t, opstest.NewMockReplicator(gomock.NewController(t)), over)

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
	over := opstest.NewMockOverReplicationCleaner(gomock.NewController(t))
	over.EXPECT().Clean(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, cfg config.ReplicationConfig, _ progress.Observer) (worker.OverReplicationSummary, error) {
			ran = cfg
			return worker.OverReplicationSummary{
				Succeeded:     4,
				CopiesRemoved: 4,
			}, nil
		}).Times(1)
	svc := replicationOver(t, opstest.NewMockReplicator(gomock.NewController(t)), over)

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

// TestReplication_RebuildQueueReportsWhatItQueued asserts a rebuild runs at the
// running factor and reports it with the keys the replicator queued, and
// declines when replication is meaningless.
func TestReplication_RebuildQueueReportsWhatItQueued(t *testing.T) {
	t.Parallel()
	repl := opstest.NewMockReplicator(gomock.NewController(t))
	repl.EXPECT().RebuildQueue(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, cfg config.ReplicationConfig) (int64, error) {
			return int64(cfg.Factor * 10), nil
		}).Times(1)
	svc := replicationOver(t, repl, opstest.NewMockOverReplicationCleaner(gomock.NewController(t)))

	res, err := svc.RebuildQueue(context.Background())
	if err != nil || res != (QueueRebuild{Factor: 2, Queued: 20}) {
		t.Errorf("RebuildQueue = %+v, %v; want factor 2 and 20 queued", res, err)
	}

	off := NewReplication(ReplicationDeps{
		Replicator: repl, OverRep: opstest.NewMockOverReplicationCleaner(gomock.NewController(t)),
		Backlog: emptyBacklog, Locker: testx.Locker{}, Config: config.NewLive(&config.Config{}),
	})
	if _, err := off.RebuildQueue(context.Background()); !errors.Is(err, ErrReplicationDisabled) {
		t.Errorf("RebuildQueue at factor 1 err = %v, want ErrReplicationDisabled", err)
	}
}

// emptyBacklog reports an empty replication queue.
var emptyBacklog = backlogFunc(func(context.Context) (core.ReplicationBacklog, error) {
	return core.ReplicationBacklog{}, nil
})

// replicationOver builds a replication service over the two stubbed workers,
// running at factor 2 over an empty backlog.
func replicationOver(t *testing.T, repl Replicator, over OverReplicationCleaner) *Replication {
	t.Helper()
	return replicationCounting(t, repl, over, emptyBacklog)
}

// replicationCounting builds a replication service whose backlog count
// comes from backlog, running at factor 2 under a lock it always takes.
func replicationCounting(t *testing.T, repl Replicator, over OverReplicationCleaner, backlog BacklogCounter) *Replication {
	t.Helper()
	return NewReplication(ReplicationDeps{
		Replicator: repl,
		OverRep:    over,
		Backlog:    backlog,
		Locker:     testx.Locker{},
		Config:     config.NewLive(&config.Config{Replication: config.ReplicationConfig{Factor: 2}}),
	})
}

// backlogFunc adapts a function to BacklogCounter.
type backlogFunc func(ctx context.Context) (core.ReplicationBacklog, error)

// CountReplicationQueue calls the function.
func (f backlogFunc) CountReplicationQueue(ctx context.Context) (core.ReplicationBacklog, error) {
	return f(ctx)
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

	svc := encryptionOver(testEncryptor(t), store)

	res, err := svc.RotateKey(context.Background(), nil, "old", false)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if res.Planned != 1 || res.Failed != 1 || res.Succeeded != 0 {
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

	declared.Store(&[]provisioning.Bucket{{Name: "later", Source: provisioning.SourceStore}})

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
		if _, err := objects.Put(context.Background(), &object.PutObjectRequest{Key: key, Body: bytes.NewReader(nil)}); !errors.Is(err, errBackend) {
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
		if res.Succeeded != 0 {
			t.Errorf("deleted = %d, want 0 when the listing never returned keys", res.Succeeded)
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
		repl := opstest.NewMockReplicator(gomock.NewController(t))
		repl.EXPECT().Replicate(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(worker.ReplicationSummary{}, errBackend).Times(1)
		svc := replicationOver(t, repl, opstest.NewMockOverReplicationCleaner(gomock.NewController(t)))

		if _, err := svc.Replicate(context.Background(), nil); !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the worker failure", err)
		}
	})

	t.Run("clean excess", func(t *testing.T) {
		t.Parallel()
		over := opstest.NewMockOverReplicationCleaner(gomock.NewController(t))
		over.EXPECT().Clean(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(worker.OverReplicationSummary{}, errBackend).Times(1)
		svc := replicationOver(t, opstest.NewMockReplicator(gomock.NewController(t)), over)

		if _, err := svc.CleanExcess(context.Background(), 0, nil); !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the worker failure", err)
		}
	})

	t.Run("count backlog", func(t *testing.T) {
		t.Parallel()
		svc := replicationCounting(t, opstest.NewMockReplicator(gomock.NewController(t)),
			opstest.NewMockOverReplicationCleaner(gomock.NewController(t)),
			backlogFunc(func(context.Context) (core.ReplicationBacklog, error) {
				return core.ReplicationBacklog{}, errBackend
			}))

		if _, err := svc.CountBacklog(context.Background()); !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the store failure", err)
		}
	})

	t.Run("rebalance", func(t *testing.T) {
		t.Parallel()
		m := opstest.NewMockRebalancer(gomock.NewController(t))
		m.EXPECT().Rebalance(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(worker.RebalanceSummary{}, errBackend).Times(1)
		svc := NewRebalance(RebalanceDeps{
			Rebalancer: m,
			Locker:     testx.Locker{},
			Config:     config.NewLive(&config.Config{}),
		})

		if _, err := svc.Run(context.Background(), nil); !errors.Is(err, errBackend) {
			t.Errorf("err = %v, want the worker failure", err)
		}
	})
}

// TestDeletePrefix_CountsAndReports asserts a prefix delete pages the listing,
// counts what it removed and separates the failures, ending each key once.
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
	res, err := objects.DeletePrefix(context.Background(), prefix, func(s progress.Step) { steps = append(steps, s) })
	if err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if res.Succeeded != 2 || res.Failed != 1 || res.Attempted != 3 {
		t.Errorf("result = %+v, want deleted=2 failed=1 total=3", res)
	}

	want := []progress.Step{
		{Label: keys[0], Phase: progress.PhaseEnd, Status: progress.StatusOK},
		{Label: keys[1], Phase: progress.PhaseEnd, Status: progress.StatusFailed},
		{Label: keys[2], Phase: progress.PhaseEnd, Status: progress.StatusOK},
	}
	if !slices.Equal(steps, want) {
		t.Errorf("steps = %+v, want %+v", steps, want)
	}
}
