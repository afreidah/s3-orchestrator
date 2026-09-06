// -------------------------------------------------------------------------------
// Object Manager Test Fleet
//
// Author: Alex Freidah
//
// Builds an object Manager over a live fleet - real backends holding real
// bytes, a real write coordinator, and the usage, routing and admission policy
// the runtime enforces - which is what the CRUD, failover, and broadcast paths
// need in order to be asserted end to end.
//
// The manager is built from object.Deps directly. Nothing here needs a
// composition root: Deps already names every collaborator the manager has, and
// reaching through the composition root would only hide that.
// -------------------------------------------------------------------------------

package object

import (
	"cmp"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// fleetTimeout and fleetCacheTTL are the defaults every fleet test runs under.
// Both are long enough that no test trips them incidentally - a timeout test
// sets its own - and the TTL is still short enough not to mask an invalidation
// bug behind a window that never closes.
const (
	fleetTimeout  = 30 * time.Second
	fleetCacheTTL = 5 * time.Second
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// fleetOpts tunes the test fleet beyond the defaults. The zero value gives
// pack routing, pending writes on, an unlimited local usage counter, and no
// encryption.
//
// Order defaults to the backend map's keys, in unspecified order. Compression
// is applied only when both Codec and Compression are set, mirroring the
// production wiring. Pending writes are on by default because that is what the
// write path ships with, so a test turns them off rather than on.
type fleetOpts struct {
	Order          []string                    // fixes fleet order, which decides placement under pack routing
	Routing        config.RoutingStrategy      // defaults to pack
	UsageLimits    map[string]core.UsageLimits // per-backend API and bandwidth caps
	MaxObjectSizes map[string]int64            // per-backend object size cap, for eligibility tests
	BackendTimeout time.Duration               // overrides the per-call bound
	AdmissionSem   chan struct{}               // bounds concurrent backend writes
	Encryptor      *encryption.Encryptor       // turns on at-rest encryption
	Codec          Codec                       // with Compression below, turns on at-rest compression
	Compression    config.CompressionConfig
	ObjectCache    objcache.ObjectCache // attaches a cache so read-through and invalidation run
	CacheTTL       time.Duration        // overrides the location-cache window, for the expiry tests
	Draining       []string             // backends the runtime should report as draining
	CopiesPerWrite int                  // above 1, a PUT places its copies itself instead of leaving them to the replicator

	QuotaBaselines map[string]core.BackendQuotaUsage // seeds the byte counter; unnamed backends are unlimited

	ParallelBroadcast            bool // fans degraded-mode reads out concurrently
	DegradedBroadcastParallelism int  // caps those concurrent probes; 0 is uncapped
	DisableDegradedReads         bool // makes the degraded path fail fast instead
}

// drainingSet reports a fixed set of backends as draining, standing in for
// drain.Manager's one-method infra.DrainChecker surface.
type drainingSet map[string]bool

// IsDraining implements infra.DrainChecker.
func (d drainingSet) IsDraining(name string) bool { return d[name] }

// fleet is an object Manager plus the collaborators a test asserts against:
// the runtime carries the usage counters, the coordinator is the one the
// manager writes through, and the integrity config is what it reads on write.
type fleet struct {
	*Manager
	Runtime   *infra.BackendRuntime
	Coord     *writepath.Coordinator
	Integrity *syncutil.AtomicConfig[config.IntegrityConfig]
}

// SetIntegrityConfig swaps the integrity settings the manager reads on write.
func (f *fleet) SetIntegrityConfig(cfg *config.IntegrityConfig) { f.Integrity.Store(cfg) }

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newFleet builds an object Manager over the supplied backends. store is the
// wide metadata store; New narrows it to Stores.
func newFleet(
	t *testing.T, store storetest.MetadataStore, backends map[string]backend.ObjectBackend, opts *fleetOpts,
) *fleet {
	t.Helper()
	if opts == nil {
		opts = &fleetOpts{}
	}

	names := opts.Order
	if names == nil {
		for name := range backends {
			names = append(names, name)
		}
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(names), opts.UsageLimits)
	rt := infra.New(&infra.Config{
		Backends:        backends,
		Order:           names,
		BackendTimeout:  cmp.Or(opts.BackendTimeout, fleetTimeout),
		Usage:           usage,
		Quota:           newFleetQuota(names, opts.QuotaBaselines),
		RoutingStrategy: cmp.Or(opts.Routing, config.RoutingPack),
		MaxObjectSizes:  opts.MaxObjectSizes,
		AdmissionSem:    opts.AdmissionSem,
	})
	rt.SetMetricsCollector(metrics.New(metrics.CollectorDeps{
		Store: store, Usage: usage, BackendNames: names,
	}))
	if len(opts.Draining) > 0 {
		d := make(drainingSet, len(opts.Draining))
		for _, name := range opts.Draining {
			d[name] = true
		}
		rt.SetDrainChecker(d)
	}

	integrity := &syncutil.AtomicConfig[config.IntegrityConfig]{}
	coord := writepath.New(rt, store)
	om := New(&Deps{
		Core:                         rt,
		BroadcastCore:                rt,
		Coord:                        coord,
		Stores:                       store,
		Encryptor:                    opts.Encryptor,
		Codec:                        opts.Codec,
		Compression:                  opts.Compression,
		LocationCache:                NewLocationCache(cmp.Or(opts.CacheTTL, fleetCacheTTL)),
		ObjectCache:                  opts.ObjectCache,
		ParallelBroadcast:            opts.ParallelBroadcast,
		CopiesPerWrite:               opts.CopiesPerWrite,
		DegradedBroadcastParallelism: opts.DegradedBroadcastParallelism,
		DisableDegradedReads:         opts.DisableDegradedReads,
		IntegrityCfg:                 integrity,
		BackendTimeout:               cmp.Or(opts.BackendTimeout, fleetTimeout),
	})
	return &fleet{Manager: om, Runtime: rt, Coord: coord, Integrity: integrity}
}

// newFleetQuota builds the byte-reservation tracker with baselines already
// primed, which production does from backend_quotas before the listener opens.
// A backend the caller said nothing about is unlimited, so a test that is not
// about quota never has one refuse a write.
func newFleetQuota(names []string, baselines map[string]core.BackendQuotaUsage) *counter.QuotaTracker {
	primed := make(map[string]core.BackendQuotaUsage, len(names))
	for _, name := range names {
		primed[name] = core.BackendQuotaUsage{BackendName: name}
	}
	for name, usage := range baselines {
		primed[name] = usage
	}
	quota := counter.NewQuotaTracker(names)
	quota.SetBaselines(primed)
	return quota
}

// newPermissiveStore returns a union store mock answering every read with an
// empty result, so a test states only the queries it asserts on.
func newPermissiveStore(t *testing.T) *storetest.MockMetadataStore {
	t.Helper()
	m := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(m)
	return m
}
