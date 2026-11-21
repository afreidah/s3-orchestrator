// -------------------------------------------------------------------------------
// Metrics Collector - Test Doubles and Plaintext Gauge Tests
//
// Author: Alex Freidah
//
// Holds the canned store and scrub sweep the collector tests read through, and
// covers the plaintext-copies gauge: published on the periodic path, and left
// as it was when the ledger read behind it fails.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// fakeStore is a metrics.Deps returning canned fleet figures: its quota stats,
// ledger, multipart counts and the scrub queue's head, or ledgerErr from the
// ledger read. The capacity lock is free unless lockBusy is set, and the queue
// counts, which only RefreshGauges reads, are left to the nil Deps.
type fakeStore struct {
	Deps
	stats     map[string]core.QuotaStat
	ledger    core.LedgerStats
	multipart map[string]int64
	head      []core.ObjectLocation
	ledgerErr error
	lockBusy  bool
	reached   *[]string
}

func (f *fakeStore) GetQuotaStats(context.Context) (map[string]core.QuotaStat, error) {
	return f.stats, nil
}

func (f *fakeStore) LedgerStats(context.Context) (core.LedgerStats, error) {
	return f.ledger, f.ledgerErr
}

func (f *fakeStore) GetActiveMultipartCounts(context.Context) (map[string]int64, error) {
	return f.multipart, nil
}

// GetLeastRecentlyScrubbedObjects returns the canned head and records the
// backends the read was scoped to in reached.
func (f *fakeStore) GetLeastRecentlyScrubbedObjects(_ context.Context, _ int, backends []string, _ time.Time) ([]core.ObjectLocation, error) {
	if f.reached != nil {
		*f.reached = backends
	}
	return f.head, nil
}

func (*fakeStore) GetUsageSince(context.Context, map[string]time.Time) (map[string]core.WorkloadUsage, error) {
	return nil, nil
}

// WithAdvisoryLock runs fn unless the lock is held elsewhere.
func (f *fakeStore) WithAdvisoryLock(ctx context.Context, _ int64, fn func(context.Context) error) (bool, error) {
	if f.lockBusy {
		return false, nil
	}
	return true, fn(ctx)
}

// scrubReach is a ScrubSweep over fixed backends, sweeping or not.
type scrubReach struct {
	reachable []string
	sweeping  bool
}

func (s scrubReach) ReachableBackends() []string { return s.reachable }
func (s scrubReach) Sweeping() bool              { return s.sweeping }

// testCollector builds a collector over d, scoping coverage to b1 with the
// sweep running when d names no scrub sweep of its own.
func testCollector(d CollectorDeps) *Collector {
	if d.Scrub == nil {
		d.Scrub = scrubReach{reachable: []string{"b1"}, sweeping: true}
	}
	return &Collector{d: d, log: slog.Default()}
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestCollector_PublishesPlaintextCopies keeps the figure refreshing on the
// periodic path. Computing it only when the web UI asks would leave a
// Prometheus-only deployment reading a gauge that never moves.
func TestCollector_PublishesPlaintextCopies(t *testing.T) {
	mc := testCollector(CollectorDeps{Store: &fakeStore{ledger: core.LedgerStats{"b1": {Plaintext: 17}}}})

	if err := mc.refreshFleet(context.Background(), nil); err != nil {
		t.Fatalf("refreshFleet: %v", err)
	}

	if got := promtest.ToFloat64(telemetry.EncryptionPlaintextCopies); got != 17 {
		t.Errorf("plaintext gauge = %v, want 17", got)
	}
}

// TestCollector_PlaintextCountFailureLeavesGauge guards against a failed count
// publishing zero. Reporting "no plaintext copies" because the query failed
// would read as a fully encrypted fleet.
func TestCollector_PlaintextCountFailureLeavesGauge(t *testing.T) {
	telemetry.EncryptionPlaintextCopies.Set(9)
	mc := testCollector(CollectorDeps{Store: &fakeStore{ledgerErr: errors.New("ledger unavailable")}})

	if err := mc.refreshFleet(context.Background(), nil); err == nil {
		t.Fatal("refreshFleet succeeded without a ledger")
	}

	if got := promtest.ToFloat64(telemetry.EncryptionPlaintextCopies); got != 9 {
		t.Errorf("gauge = %v after a failed count, want the previous value 9", got)
	}
}
