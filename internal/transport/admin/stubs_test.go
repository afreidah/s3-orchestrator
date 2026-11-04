// -------------------------------------------------------------------------------
// Admin Handler - Worker Stubs
//
// Author: Alex Freidah
//
// Constructors that dress the generated ops mocks in the behaviour the handler
// tests need: a worker that reports N units of work, replays them through the
// progress observer, or fails. The mocks themselves are generated from the
// consumer-defined interfaces in deps.go - what lives here is only the
// per-test wiring, so a test reads as one line of intent rather than a
// hand-rolled type.
//
// Every stub is permissive (.AnyTimes()) on purpose: these are the
// collaborators a handler test drives past, not the thing it asserts on. A
// test that does care how a worker was called states its own expectation on
// the returned mock.
// -------------------------------------------------------------------------------

package admin

import (
	"cmp"
	"context"
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// trackN reports n successful units through the observer, so a streaming
// endpoint has progress lines to render.
func trackN(observer progress.Observer, n int, label func(int) string) {
	for i := range n {
		progress.Track(observer, label(i), func() string { return progress.StatusOK })
	}
}

// fixedKey labels every tracked unit identically, for tests that assert on the
// count rather than on which object moved.
func fixedKey(string) func(int) string {
	return func(int) string { return "fake-key" }
}

// backendOpsStub configures the BackendOps mocks. The admin transport reads
// only the usage counters; the operations layer also reads the integrity
// settings, so one stub configures both mocks.
type backendOpsStub struct {
	flushErr     error
	integrity    *config.IntegrityConfig
	reconcileMap map[string]int64
	reconcileErr error
}

// newBackendOps builds a permissive BackendOps mock from cfg.
func newBackendOps(t *testing.T, cfg backendOpsStub) *MockBackendOps {
	t.Helper()
	m := NewMockBackendOps(gomock.NewController(t))
	m.EXPECT().FlushUsage(gomock.Any()).Return(cfg.flushErr).AnyTimes()
	m.EXPECT().ReconcileUsage(gomock.Any()).Return(cfg.reconcileMap, cfg.reconcileErr).AnyTimes()
	return m
}

// newOpsMover builds the mover a rewrite pass hands each copy to. It expects
// no calls: the runtime these tests use resolves no backend, so a pass fails
// each copy before it would move it.
func newOpsMover(t *testing.T) *opstest.MockMover {
	t.Helper()
	return opstest.NewMockMover(gomock.NewController(t))
}

// integrityWith installs integrity operations built from the given stubs, for
// the handler tests that drive scrub, per-key verification, or backfill. The
// stub's integrity settings are the running config's.
func integrityWith(t *testing.T, h *Handler, be backendOpsStub, sc *scrubberStub) {
	t.Helper()
	running := &config.Config{}
	if be.integrity != nil {
		running.Integrity = *be.integrity
	}
	h.d.Ops.Integrity = ops.NewIntegrity(ops.IntegrityDeps{
		Scrubber: newScrubber(t, sc),
		Locker:   testx.Locker{},
		Config:   config.NewLive(running),
	})
}

// replicationWith installs replication operations built from the given stubs.
// The half a test does not exercise still needs a stub, since one service
// serves both the copy-creating and the surplus-removing endpoints. Whichever
// stub sets a replication config supplies the running one.
func replicationWith(t *testing.T, h *Handler, repl replicatorStub, over overRepStub) {
	t.Helper()
	running := &config.Config{}
	if cfg := cmp.Or(repl.cfg, over.cfg); cfg != nil {
		running.Replication = *cfg
	}
	h.d.Ops.Replication = ops.NewReplication(ops.ReplicationDeps{
		Replicator: newReplicator(t, repl),
		OverRep:    newOverRep(t, over),
		Backlog:    backlogStub{over: over.count, err: over.countErr},
		Config:     config.NewLive(running),
	})
}

// backendsConfig is a running config declaring a backend under each name.
func backendsConfig(names ...string) *config.Live {
	cfg := &config.Config{}
	for _, name := range names {
		cfg.Backends = append(cfg.Backends, config.BackendConfig{Name: name})
	}
	return config.NewLive(cfg)
}

// backlogStub reports a fixed over-replicated count, or err.
type backlogStub struct {
	over int64
	err  error
}

// CountReplicationBacklog returns the stub's count.
func (b backlogStub) CountReplicationBacklog(context.Context, int) (core.ReplicationBacklog, error) {
	return core.ReplicationBacklog{Over: b.over}, b.err
}

// rebalanceWith installs rebalance operations built from cfg, so a test can
// read back the config the cycle actually ran with. A nil cfg stands for a
// deployment whose worker pool was never wired.
func rebalanceWith(t *testing.T, h *Handler, cfg *rebalancerStub) {
	t.Helper()
	deps := ops.RebalanceDeps{Config: config.NewLive(&config.Config{})}
	if cfg != nil {
		deps.Rebalancer = newRebalancer(t, cfg)
	}
	h.d.Ops.Rebalance = ops.NewRebalance(deps)
}

// encryptionWith installs encryption operations over one store, for the tests
// that drive the bulk rewrite and key rotation endpoints.
func encryptionWith(t *testing.T, h *Handler, enc *encryption.Encryptor, store ops.RewriteStore) {
	t.Helper()
	h.d.Ops.Encryption = ops.NewEncryption(ops.EncryptionDeps{
		Encryptor: enc,
		Store:     store,
		Mover:     newOpsMover(t),
	})
}

// compressionWith installs a compression service over the given codec and
// store, so a test drives one branch of the bulk passes without standing up the
// rest of the operations layer.
func compressionWith(t *testing.T, h *Handler, codec *compression.Codec, store ops.RewriteStore) {
	t.Helper()
	h.d.Ops.Compression = ops.NewCompression(&ops.CompressionDeps{
		Codec:  codec,
		Config: config.CompressionConfig{Enabled: true, Level: "default", MinRatio: 0.95},
		Store:  store,
		Mover:  newOpsMover(t),
	})
}

// newDashboardOps builds a permissive DashboardReader mock returning data or err.
func newDashboardOps(t *testing.T, data *dashboard.Data, err error) *MockDashboardReader {
	t.Helper()
	m := NewMockDashboardReader(gomock.NewController(t))
	m.EXPECT().GetStatus(gomock.Any()).Return(data, err).AnyTimes()
	return m
}

// replicatorStub configures the Replicator mock. failed stands for the
// objects the cycle could not bring up to factor.
type replicatorStub struct {
	cfg     *config.ReplicationConfig
	created int
	failed  int
	err     error
}

// newReplicator builds a Replicator mock that reports cfg.created copies.
func newReplicator(t *testing.T, cfg replicatorStub) *opstest.MockReplicator {
	t.Helper()
	m := opstest.NewMockReplicator(gomock.NewController(t))
	m.EXPECT().Replicate(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ config.ReplicationConfig, observer progress.Observer) (worker.ReplicationSummary, error) {
			trackN(observer, cfg.created, fixedKey(""))
			return worker.ReplicationSummary{
				Succeeded: cfg.created, Failed: cfg.failed,
				CopiesCreated: cfg.created,
			}, cfg.err
		}).AnyTimes()
	return m
}

// rebalancerStub configures the Rebalancer mock. gotCfg captures the config
// the handler ran with, so the default-fallback can be asserted.
type rebalancerStub struct {
	moved  int
	skip   string
	err    error
	gotCfg *config.RebalanceConfig
}

// newRebalancer builds a Rebalancer mock that reports cfg.moved moves, or
// the configured skip reason.
func newRebalancer(t *testing.T, cfg *rebalancerStub) *opstest.MockRebalancer {
	t.Helper()
	m := opstest.NewMockRebalancer(gomock.NewController(t))
	m.EXPECT().Rebalance(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, ran config.RebalanceConfig, observer progress.Observer) (worker.RebalanceSummary, error) {
			cfg.gotCfg = &ran
			if cfg.skip != "" {
				return worker.RebalanceSummary{SkipReason: cfg.skip}, cfg.err
			}
			trackN(observer, cfg.moved, func(i int) string { return fmt.Sprintf("obj-%d  src -> dst", i) })
			return worker.RebalanceSummary{Succeeded: cfg.moved}, cfg.err
		}).AnyTimes()
	return m
}

// overRepStub configures the OverReplicationCleaner mock. failed stands for the
// objects whose surplus the cycle could not remove.
type overRepStub struct {
	cfg      *config.ReplicationConfig
	count    int64
	countErr error
	cleaned  int
	failed   int
	cleanErr error
}

// newOverRep builds an OverReplicationCleaner mock reporting cfg.count pending and
// cleaning cfg.cleaned copies.
func newOverRep(t *testing.T, cfg overRepStub) *opstest.MockOverReplicationCleaner {
	t.Helper()
	m := opstest.NewMockOverReplicationCleaner(gomock.NewController(t))
	m.EXPECT().Clean(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ config.ReplicationConfig, observer progress.Observer) (worker.OverReplicationSummary, error) {
			trackN(observer, cfg.cleaned, fixedKey(""))
			return worker.OverReplicationSummary{
				Succeeded: cfg.cleaned, Failed: cfg.failed,
				CopiesRemoved: cfg.cleaned,
			}, cfg.cleanErr
		}).AnyTimes()
	return m
}

// scrubberStub configures the ScrubberOps mock. backfillBacklog is how many
// copies a backfill has to hash; a run hashes them up to its cap, and
// backfillReq records the request it ran with.
type scrubberStub struct {
	scrubChecked    int
	scrubFailed     int
	scrubSkipped    int
	scrubDeferred   int
	scrubKeyCopies  []worker.CopyVerification
	scrubKeyErr     error
	backfillBacklog int
	backfillReq     worker.BackfillRequest
	unreadable      []core.ObjectLocation
}

// newScrubber builds a ScrubberOps mock from cfg.
func newScrubber(t *testing.T, cfg *scrubberStub) *opstest.MockScrubberOps {
	t.Helper()
	m := opstest.NewMockScrubberOps(gomock.NewController(t))
	m.EXPECT().Scrub(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int, _ string, observer progress.Observer) (batch.Summary, error) {
			trackN(observer, cfg.scrubChecked, fixedKey(""))
			return batch.Summary{
				Attempted: cfg.scrubChecked,
				Succeeded: cfg.scrubChecked - cfg.scrubFailed,
				Failed:    cfg.scrubFailed,
				Skipped:   cfg.scrubSkipped,
				Deferred:  cfg.scrubDeferred,
			}, nil
		}).AnyTimes()
	m.EXPECT().ScrubKey(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string) ([]worker.CopyVerification, error) {
			return cfg.scrubKeyCopies, cfg.scrubKeyErr
		}).AnyTimes()
	m.EXPECT().Backfill(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, req worker.BackfillRequest, observer progress.Observer) (worker.BackfillResult, error) {
			cfg.backfillReq = req
			n := cfg.backfillBacklog
			if req.MaxObjects > 0 {
				n = min(n, req.MaxObjects)
			}
			cfg.backfillBacklog -= n
			trackN(observer, n, fixedKey(""))
			return worker.BackfillResult{
				Summary: batch.Summary{Attempted: n, Succeeded: n},
				Done:    cfg.backfillBacklog == 0,
			}, nil
		}).AnyTimes()
	m.EXPECT().ListUnreadable(gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, int) ([]core.ObjectLocation, int64, error) {
			return cfg.unreadable, int64(len(cfg.unreadable)), nil
		}).AnyTimes()
	// Purges every unreadable copy in one pass, so the next pass finds none.
	m.EXPECT().PurgeUnreadable(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int, observer progress.Observer) (batch.Summary, error) {
			n := len(cfg.unreadable)
			trackN(observer, n, fixedKey(""))
			cfg.unreadable = nil
			return batch.Summary{Attempted: n, Succeeded: n}, nil
		}).AnyTimes()
	return m
}

// reconciled is a reconcile result that imported and removed rows across
// scanned backends, failed of them partway.
func reconciled(imported, removed, scanned, failed int) *worker.ReconcileResult {
	return &worker.ReconcileResult{
		Summary:  batch.Summary{Planned: scanned, Attempted: scanned, Succeeded: scanned - failed, Failed: failed},
		Imported: imported,
		Removed:  removed,
	}
}

// newReconciler builds a Reconciler mock returning result or err, reporting one
// backend step to an observer when the caller streams. result is nil when the
// mock fails.
func newReconciler(t *testing.T, result *worker.ReconcileResult, err error) *MockReconciler {
	t.Helper()
	m := NewMockReconciler(gomock.NewController(t))
	m.EXPECT().Reconcile(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, observer progress.Observer) (worker.ReconcileResult, error) {
			if err != nil {
				return worker.ReconcileResult{}, err
			}
			progress.Track(observer, "fake-backend", func() string { return progress.StatusOK })
			return *result, nil
		}).AnyTimes()
	return m
}

// declaredBuckets builds the live bucket set the object operations resolve a
// key against, holding the named buckets.
func declaredBuckets(names ...string) *provisioning.Declared {
	buckets := make([]provisioning.Bucket, 0, len(names))
	for _, n := range names {
		buckets = append(buckets, provisioning.Bucket{Name: n, Source: provisioning.SourceStore})
	}
	d := provisioning.NewDeclared()
	d.Store(&buckets)
	return d
}
