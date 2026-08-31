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
	"fmt"
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
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// testBucket is the virtual bucket every key in these tests lives under.
const testBucket = "bucket"

// fakeBackend is a minimal in-memory ObjectBackend for the bulk-rewrite happy
// path. GetObject returns a fixed payload and PutObject records the call so a
// test can assert the rewrite reached the upload step.
type fakeBackend struct {
	payload []byte
	puts    atomic.Int64
	gets    atomic.Int64
	// lastPut is what the most recent upload carried, so a test can assert on
	// the bytes a rewrite produced rather than only on the call count.
	lastPut []byte
}

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

// emptyEncAdmin is a minimal EncryptionAdmin stub: every listing is empty and
// every mutator is a no-op, so a pass runs to completion with nothing to do.
type emptyEncAdmin struct{}

// ListEncryptedLocations lists no encrypted locations.
func (emptyEncAdmin) ListEncryptedLocations(_ context.Context, _ string, _, _ int) ([]core.EncryptedLocation, error) {
	return nil, nil
}

// UpdateEncryptionKey is a no-op.
func (emptyEncAdmin) UpdateEncryptionKey(_ context.Context, _, _ string, _ []byte, _ string) error {
	return nil
}

// CountUnencryptedLocations reports no plaintext copies.
func (emptyEncAdmin) CountUnencryptedLocations(_ context.Context) (int64, error) { return 0, nil }

// ListUnencryptedLocations lists no plaintext locations.
func (emptyEncAdmin) ListUnencryptedLocations(_ context.Context, _ int, _ core.Cursor) ([]core.UnencryptedLocation, error) {
	return nil, nil
}

// MarkObjectEncrypted is a no-op.
func (emptyEncAdmin) MarkObjectEncrypted(_ context.Context, _ *core.EncryptedUpdate) error {
	return nil
}

// ListAllEncryptedLocations lists no encrypted locations.
func (emptyEncAdmin) ListAllEncryptedLocations(_ context.Context, _ int, _ core.Cursor) ([]core.DecryptableLocation, error) {
	return nil, nil
}

// MarkObjectDecrypted is a no-op.
func (emptyEncAdmin) MarkObjectDecrypted(_ context.Context, _, _ string, _ int64) error { return nil }

// rowEncAdmin serves exactly one plaintext location, so the bulk-rewrite loop
// processes a single object and then terminates.
type rowEncAdmin struct {
	emptyEncAdmin
	row     core.UnencryptedLocation
	markErr error
	served  atomic.Bool
	marked  atomic.Bool
}

// ListUnencryptedLocations serves the single row once.
func (r *rowEncAdmin) ListUnencryptedLocations(_ context.Context, _ int, _ core.Cursor) ([]core.UnencryptedLocation, error) {
	if r.served.Swap(true) {
		return nil, nil
	}
	return []core.UnencryptedLocation{r.row}, nil
}

// MarkObjectEncrypted records that the post-encrypt metadata update ran, or
// fails when the fixture is configured to.
func (r *rowEncAdmin) MarkObjectEncrypted(_ context.Context, _ *core.EncryptedUpdate) error {
	if r.markErr != nil {
		return r.markErr
	}
	r.marked.Store(true)
	return nil
}

// testServices builds the operations layer over a real manager and workers
// backed by the union store mock, with replication at factor 1, integrity off
// and no encryptor - the conditions every declined path checks. Backends maps
// each name to a fake so a rewrite can run end to end.
func testServices(t *testing.T, backends map[string]s3be.ObjectBackend, enc *encryption.Encryptor, encStore EncryptionStore) *Services {
	t.Helper()
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

	var store EncryptionStore = mock
	if encStore != nil {
		store = encStore
	}
	return New(&Deps{
		Objects:      st.Objects,
		Store:        mock,
		Encryptor:    enc,
		EncStore:     store,
		Runtime:      st.Runtime,
		Usage:        st.Runtime.Usage(),
		IntegrityCfg: st.IntegrityCfg,
		Replicator:   workers.Replicator,
		OverRep:      workers.OverReplicationCleaner,
		Rebalancer:   workers.Rebalancer,
		Scrubber:     workers.Scrubber,
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
		Config:  NewConfigStore(&config.Config{Buckets: []config.BucketConfig{{Name: testBucket}}}),
	}), api
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
	if res.Moved != 2 {
		t.Errorf("Moved = %d, want 2", res.Moved)
	}
}

// TestScrub_SkippedWhenIntegrityDisabled asserts a scrub declines rather than
// reporting a pass that verified nothing.
func TestScrub_SkippedWhenIntegrityDisabled(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)

	res, err := svc.Integrity.Scrub(context.Background(), 0, nil)
	assertSkipped(t, err)
	if res.Checked != 0 || res.Failed != 0 {
		t.Errorf("Checked=%d Failed=%d, want both 0", res.Checked, res.Failed)
	}
}

// TestScrub_EmptyStore asserts an enabled scrub over an empty store completes
// with zero counts.
func TestScrub_EmptyStore(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)
	enableIntegrity(t, svc)

	res, err := svc.Integrity.Scrub(context.Background(), 0, nil)
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if res.Checked != 0 || res.Failed != 0 {
		t.Errorf("Checked=%d Failed=%d, want both 0", res.Checked, res.Failed)
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

	res, err := svc.Integrity.BackfillChecksums(context.Background(), 0, 0, 0, nil)
	assertSkipped(t, err)
	if res.Processed != 0 {
		t.Errorf("Processed = %d, want 0", res.Processed)
	}
}

// TestBackfillChecksums_EmptyStore asserts an enabled backfill with no backlog
// reports a drained run.
func TestBackfillChecksums_EmptyStore(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)
	enableIntegrity(t, svc)

	res, err := svc.Integrity.BackfillChecksums(context.Background(), 0, 0, 0, nil)
	if err != nil {
		t.Fatalf("BackfillChecksums: %v", err)
	}
	if res.Processed != 0 {
		t.Errorf("Processed = %d, want 0", res.Processed)
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

	res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0)
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

	res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("EncryptExisting: %v", err)
	}
	if res.Total != 0 || res.Succeeded != 0 || res.Failed != 0 {
		t.Errorf("counts = (%d, %d, %d), want all 0", res.Succeeded, res.Failed, res.Total)
	}
}

// TestEncryptExisting_OneRow asserts the whole rewrite runs for a single
// object: list, download, encrypt, re-upload, then mark the row encrypted.
func TestEncryptExisting_OneRow(t *testing.T) {
	t.Parallel()
	fake := &fakeBackend{payload: []byte("hello world")}
	encStore := &rowEncAdmin{row: core.UnencryptedLocation{
		ObjectKey:   testBucket + "/file.txt",
		BackendName: "backend-a",
		SizeBytes:   int64(len(fake.payload)),
	}}
	svc := testServices(t, map[string]s3be.ObjectBackend{"backend-a": fake}, testEncryptor(t), encStore)

	res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("EncryptExisting: %v", err)
	}
	if res.Total != 1 || res.Succeeded != 1 || res.Failed != 0 {
		t.Errorf("counts = (%d, %d, %d), want (1, 0, 1)", res.Succeeded, res.Failed, res.Total)
	}
	if !encStore.marked.Load() {
		t.Error("MarkObjectEncrypted was not called; the metadata update did not run")
	}
	if fake.puts.Load() != 1 {
		t.Errorf("PutObject calls = %d, want 1", fake.puts.Load())
	}
}

// TestBulkRewriteAdapters exercises the row adapters that let one rewrite
// driver serve both directions.
func TestBulkRewriteAdapters(t *testing.T) {
	t.Parallel()

	er := &encryptRow{ObjectKey: "k", BackendName: "b", SizeBytes: 42}
	if er.rewriteKey() != "k" || er.rewriteBackend() != "b" || er.rewriteSize() != 42 {
		t.Errorf("encryptRow accessors wrong: %+v", er)
	}

	dr := &decryptRow{ObjectKey: "x", BackendName: "y", SizeBytes: 7}
	if dr.rewriteKey() != "x" || dr.rewriteBackend() != "y" || dr.rewriteSize() != 7 {
		t.Errorf("decryptRow accessors wrong: %+v", dr)
	}
}

// decryptRowStore serves exactly one encrypted location, so the reverse
// rewrite processes a single object and then terminates.
type decryptRowStore struct {
	emptyEncAdmin
	row    core.DecryptableLocation
	served atomic.Bool
	marked atomic.Bool
}

// ListAllEncryptedLocations serves the single row once.
func (d *decryptRowStore) ListAllEncryptedLocations(_ context.Context, _ int, _ core.Cursor) ([]core.DecryptableLocation, error) {
	if d.served.Swap(true) {
		return nil, nil
	}
	return []core.DecryptableLocation{d.row}, nil
}

// MarkObjectDecrypted records that the post-decrypt metadata update ran.
func (d *decryptRowStore) MarkObjectDecrypted(_ context.Context, _, _ string, _ int64) error {
	d.marked.Store(true)
	return nil
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
	store := &decryptRowStore{row: core.DecryptableLocation{
		ObjectKey:     testBucket + "/file.txt",
		BackendName:   "backend-a",
		SizeBytes:     int64(len(ciphertext)),
		PlaintextSize: int64(len(payload)),
		EncryptionKey: keyData,
		KeyID:         keyID,
	}}
	svc := testServices(t, map[string]s3be.ObjectBackend{"backend-a": fake}, enc, store)

	res, err := svc.Encryption.DecryptExisting(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("DecryptExisting: %v", err)
	}
	if res.Total != 1 || res.Succeeded != 1 || res.Failed != 0 {
		t.Errorf("res = %+v, want total=1 succeeded=1 failed=0", res)
	}
	if !store.marked.Load() {
		t.Error("MarkObjectDecrypted was not called; the metadata update did not run")
	}
	if fake.puts.Load() != 1 {
		t.Errorf("PutObject calls = %d, want 1", fake.puts.Load())
	}
}

// TestRotateKey_RewrapsEveryLocation asserts a rotation re-wraps the DEK and
// records the new key material, leaving the object bytes untouched.
func TestRotateKey_RewrapsEveryLocation(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t)
	keyData, keyID, _ := sealed(t, enc, []byte("hello world"))

	var updated bool
	store := opstest.NewMockEncryptionStore(gomock.NewController(t))
	first := store.EXPECT().ListEncryptedLocations(gomock.Any(), keyID, rotateBatchSize, 0).
		Return([]core.EncryptedLocation{
			{ObjectKey: testBucket + "/file.txt", BackendName: "b1", EncryptionKey: keyData, KeyID: keyID},
		}, nil).Times(1)
	store.EXPECT().ListEncryptedLocations(gomock.Any(), keyID, gomock.Any(), gomock.Any()).
		Return(nil, nil).After(first).AnyTimes()
	store.EXPECT().UpdateEncryptionKey(gomock.Any(), testBucket+"/file.txt", "b1", gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, newKeyData []byte, _ string) error {
			updated = len(newKeyData) > 0
			return nil
		}).Times(1)

	svc := NewEncryption(EncryptionDeps{
		Encryptor: enc,
		Store:     store,
		Runtime:   opstest.NewMockRuntimeOps(gomock.NewController(t)),
		Usage:     opstest.NewMockUsageGate(gomock.NewController(t)),
	})

	res, err := svc.RotateKey(context.Background(), keyID)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if res.Rotated != 1 || res.Failed != 0 || res.Total != 1 {
		t.Errorf("res = %+v, want rotated=1 failed=0 total=1", res)
	}
	if !updated {
		t.Error("UpdateEncryptionKey received no new key material")
	}
}

// TestEncryptExisting_FailureModesAreCountedPerObject asserts each way one
// object can fail leaves the pass running and the row untouched: the read, the
// re-upload, and the metadata update each fail on their own.
func TestEncryptExisting_FailureModesAreCountedPerObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		method  backendtest.Method
		markErr error
	}{
		{name: "download fails", method: backendtest.MethodGet},
		{name: "re-upload fails", method: backendtest.MethodPut},
		{name: "metadata update fails", markErr: errors.New("ledger unavailable")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			be := backendtest.New(backendtest.NewInMemory())
			if tc.markErr == nil {
				be.SetErr(tc.method, backendtest.ErrSimulatedBackendFailure)
			}
			store := &rowEncAdmin{
				markErr: tc.markErr,
				row: core.UnencryptedLocation{
					ObjectKey:   testBucket + "/file.txt",
					BackendName: "backend-a",
					SizeBytes:   11,
				},
			}
			svc := testServices(t, map[string]s3be.ObjectBackend{"backend-a": be}, testEncryptor(t), store)

			res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0)
			if err != nil {
				t.Fatalf("EncryptExisting: %v", err)
			}
			if res.Total != 1 || res.Failed != 1 || res.Succeeded != 0 {
				t.Errorf("res = %+v, want total=1 failed=1 succeeded=0", res)
			}
			if tc.markErr == nil && store.marked.Load() {
				t.Error("metadata was updated for an object that was never rewritten")
			}
		})
	}
}

// TestRotateKey_SkippedWithoutEncryptor asserts rotation declines rather than
// reporting an empty run when encryption is off.
func TestRotateKey_SkippedWithoutEncryptor(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)

	_, err := svc.Encryption.RotateKey(context.Background(), "old-key")
	assertSkipped(t, err)
}

// TestRotateKey_RejectsEmptyKeyID asserts the old key id is required, since
// rotation is scoped to the objects sealed under it.
func TestRotateKey_RejectsEmptyKeyID(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, testEncryptor(t), emptyEncAdmin{})

	if _, err := svc.Encryption.RotateKey(context.Background(), ""); !errors.Is(err, ErrKeyIDRequired) {
		t.Errorf("err = %v, want ErrKeyIDRequired", err)
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
	icfg := opstest.NewMockIntegrityConfigLoader(gomock.NewController(t))
	icfg.EXPECT().Load().
		Return(&config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}).AnyTimes()
	return NewIntegrity(IntegrityDeps{Scrubber: scrubber, IntegrityCfg: icfg})
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
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().Backfill(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, batchSize, offset int, observer progress.Observer) (worker.WorkSummary, int) {
			for i := range batchSize {
				progress.Track(observer, fmt.Sprintf("key-%d-%d", offset, i), func() string { return progress.StatusOK })
			}
			return worker.WorkSummary{Attempted: batchSize, Succeeded: batchSize}, offset + batchSize
		}).AnyTimes()

	res, err := integrityOver(t, scrubber).BackfillChecksums(context.Background(), 10, 25, 0, nil)
	if err != nil {
		t.Fatalf("BackfillChecksums: %v", err)
	}
	if res.Processed < 25 {
		t.Errorf("Processed = %d, want at least the 25 requested", res.Processed)
	}
	if res.Done {
		t.Error("Done = true, want false when the cap stopped the run short of the backlog")
	}
}

// TestEncryptExisting_CountsUnreachableBackend asserts one location whose
// backend is gone fails on its own rather than ending the pass.
func TestEncryptExisting_CountsUnreachableBackend(t *testing.T) {
	t.Parallel()
	encStore := &rowEncAdmin{row: core.UnencryptedLocation{
		ObjectKey:   testBucket + "/file.txt",
		BackendName: "missing-backend",
		SizeBytes:   11,
	}}
	svc := testServices(t, map[string]s3be.ObjectBackend{}, testEncryptor(t), encStore)

	res, err := svc.Encryption.EncryptExisting(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("EncryptExisting: %v", err)
	}
	if res.Total != 1 || res.Failed != 1 || res.Succeeded != 0 {
		t.Errorf("res = %+v, want total=1 failed=1 succeeded=0", res)
	}
	if encStore.marked.Load() {
		t.Error("metadata was updated for an object that was never rewritten")
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
		ListObjectsDelimited(gomock.Any(), "", "/", "", defaultListMaxKeys).
		Return(&core.ListDelimitedResult{CommonPrefixes: []string{testBucket + "/"}}, nil).
		Times(1)
	objects := NewObjects(ObjectsDeps{
		Objects: opstest.NewMockObjectAPI(gomock.NewController(t)),
		Store:   store,
		Config:  NewConfigStore(&config.Config{}),
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
		ListObjects(gomock.Any(), testBucket+"/dir/", "", defaultListMaxKeys).
		Return(&core.ListObjectsResult{
			Objects:     []core.ObjectLocation{{ObjectKey: testBucket + "/dir/a"}, {ObjectKey: testBucket + "/dir/b"}},
			IsTruncated: true,
		}, nil).
		Times(1)
	objects := NewObjects(ObjectsDeps{
		Objects: opstest.NewMockObjectAPI(gomock.NewController(t)),
		Store:   store,
		Config:  NewConfigStore(&config.Config{}),
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
		Config:  NewConfigStore(&config.Config{}),
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

	res, err := svc.Encryption.DecryptExisting(context.Background(), nil, 0)
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
	store := opstest.NewMockEncryptionStore(gomock.NewController(t))
	first := store.EXPECT().ListEncryptedLocations(gomock.Any(), "old", rotateBatchSize, 0).
		Return([]core.EncryptedLocation{
			{ObjectKey: "k1", BackendName: "b1", EncryptionKey: []byte{0x01}, KeyID: "old"},
		}, nil).Times(1)
	store.EXPECT().ListEncryptedLocations(gomock.Any(), "old", gomock.Any(), gomock.Any()).
		Return(nil, nil).After(first).AnyTimes()

	svc := NewEncryption(EncryptionDeps{
		Encryptor: testEncryptor(t),
		Store:     store,
		Runtime:   opstest.NewMockRuntimeOps(gomock.NewController(t)),
		Usage:     opstest.NewMockUsageGate(gomock.NewController(t)),
	})

	res, err := svc.RotateKey(context.Background(), "old")
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if res.Total != 1 || res.Failed != 1 || res.Rotated != 0 {
		t.Errorf("res = %+v, want total=1 failed=1 rotated=0", res)
	}
}

// TestConfigStore_UpdateReachesOperations asserts a reload changes what a
// later run reads, which is why the operations hold the store rather than a
// snapshot.
func TestConfigStore_UpdateReachesOperations(t *testing.T) {
	t.Parallel()
	svc := testServices(t, map[string]s3be.ObjectBackend{}, nil, nil)

	if err := svc.Objects.Delete(context.Background(), "later/file.txt"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("err = %v, want ErrInvalidKey before the reload", err)
	}

	svc.UpdateConfig(&config.Config{Buckets: []config.BucketConfig{{Name: "later"}}})

	if err := svc.Objects.Delete(context.Background(), "later/file.txt"); errors.Is(err, ErrInvalidKey) {
		t.Error("key still rejected after the bucket was added by a reload")
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
		Config:  &ConfigStore{},
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
	scrubber.EXPECT().Backfill(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, batchSize, offset int, observer progress.Observer) (worker.WorkSummary, int) {
			progress.Track(observer, "key", func() string { return progress.StatusOK })
			cancel()
			return worker.WorkSummary{Attempted: 1, Succeeded: 1}, offset + batchSize
		}).Times(1)

	res, err := integrityOver(t, scrubber).BackfillChecksums(ctx, 10, 0, time.Millisecond, nil)
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
	var calls atomic.Int64

	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().Backfill(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, batchSize, offset int, observer progress.Observer) (worker.WorkSummary, int) {
			progress.Track(observer, "key", func() string { return progress.StatusOK })
			if calls.Add(1) == 1 {
				return worker.WorkSummary{Attempted: 1, Succeeded: 1}, offset + batchSize
			}
			return worker.WorkSummary{Attempted: 1, Succeeded: 1}, 0
		}).Times(2)

	start := time.Now()
	res, err := integrityOver(t, scrubber).BackfillChecksums(context.Background(), 10, 0, 20*time.Millisecond, nil)
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
		Return(&object.ListObjectsV2Result{Objects: []core.ObjectLocation{
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
