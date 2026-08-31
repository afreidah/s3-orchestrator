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

const (
	// fleetTimeout bounds a backend call in the test fleet. Long enough that
	// no test trips it incidentally; timeout tests set their own.
	fleetTimeout = 30 * time.Second
	// fleetCacheTTL is the location-cache window. Long enough that a test
	// sees its own writes, short enough not to mask an invalidation bug.
	fleetCacheTTL = 5 * time.Second
)

// fleetOpts tunes the test fleet beyond the defaults. The zero value gives
// pack routing, pending writes on, an unlimited local usage counter, and no
// encryption.
type fleetOpts struct {
	// Order fixes the fleet order, which decides placement under pack
	// routing. Defaults to the backend map's keys, in unspecified order.
	Order []string
	// Routing overrides the placement strategy. Defaults to pack.
	Routing config.RoutingStrategy
	// UsageLimits applies per-backend API/bandwidth caps.
	UsageLimits map[string]core.UsageLimits
	// MaxObjectSizes caps per-backend object size, for eligibility tests.
	MaxObjectSizes map[string]int64
	// BackendTimeout overrides the per-call bound.
	BackendTimeout time.Duration
	// AdmissionSem bounds concurrent backend writes.
	AdmissionSem chan struct{}
	// Encryptor turns on at-rest encryption.
	Encryptor *encryption.Encryptor
	// Codec and Compression turn on at-rest compression. Compression is
	// applied only when both are set, mirroring production wiring.
	Codec       Codec
	Compression config.CompressionConfig
	// ObjectCache attaches a cache so read-through and invalidation run.
	ObjectCache objcache.ObjectCache
	// ParallelBroadcast fans degraded-mode reads out concurrently.
	ParallelBroadcast bool
	// DegradedBroadcastParallelism caps those concurrent probes. 0 = uncapped.
	DegradedBroadcastParallelism int
	// DisableDegradedReads makes the degraded path fail fast instead.
	DisableDegradedReads bool
	// PendingDisabled turns the coordinator's pending-write pattern off; it
	// is on by default because that is what the write path ships with.
	PendingDisabled bool
	// Draining lists backends the runtime should report as draining.
	Draining []string
	// CacheTTL overrides the location-cache window, for the expiry tests.
	CacheTTL time.Duration
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
	coord := writepath.New(rt, store, !opts.PendingDisabled)
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
		DegradedBroadcastParallelism: opts.DegradedBroadcastParallelism,
		DisableDegradedReads:         opts.DisableDegradedReads,
		IntegrityCfg:                 integrity,
		BackendTimeout:               cmp.Or(opts.BackendTimeout, fleetTimeout),
	})
	return &fleet{Manager: om, Runtime: rt, Coord: coord, Integrity: integrity}
}

// newPermissiveStore returns a union store mock answering every read with an
// empty result, so a test states only the queries it asserts on.
func newPermissiveStore(t *testing.T) *storetest.MockMetadataStore {
	t.Helper()
	m := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(m)
	return m
}
