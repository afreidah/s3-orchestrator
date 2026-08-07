// -------------------------------------------------------------------------------
// Multipart Test Fleet
//
// Author: Alex Freidah
//
// Builds a multipart Manager over a live fleet - real backends holding real
// bytes, a real write coordinator, and the usage and admission policy the
// runtime enforces - which is what the upload lifecycle needs in order to be
// asserted end to end.
//
// The manager is built from multipart.Deps directly. Nothing here needs a
// BackendManager: Deps already names every collaborator the manager has, and
// reaching through the composition root would only hide that.
// -------------------------------------------------------------------------------

package multipart

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
	// no test trips it incidentally.
	fleetTimeout = 30 * time.Second
	// fleetDEKTTL keeps per-upload data keys alive for a whole test.
	fleetDEKTTL = time.Hour
)

// fleetOpts tunes the test fleet beyond the defaults. The zero value gives
// pack routing, an unlimited local usage counter, and no encryption.
type fleetOpts struct {
	// Order fixes the fleet order. Defaults to the backend map's keys.
	Order []string
	// Encryptor turns on at-rest encryption for the upload.
	Encryptor *encryption.Encryptor
	// ObjectCache attaches a cache so completion-time invalidation runs.
	ObjectCache objcache.ObjectCache
	// PendingEnabled turns on the coordinator's pending-write pattern.
	PendingEnabled bool
	// AdmissionSem bounds concurrent backend writes, for admission tests.
	AdmissionSem chan struct{}
	// BackendTimeout overrides the per-call bound. Defaults to fleetTimeout,
	// which no test trips by accident; timeout tests set their own.
	BackendTimeout time.Duration
	// EnforceMinPartSize turns on the S3 5 MiB non-final part floor. Off by
	// default because most fixtures here upload 3-byte parts.
	EnforceMinPartSize bool
}

// fleet is a multipart Manager plus the collaborators a test asserts against:
// the runtime carries the usage counters, and the integrity config is the one
// the manager reads at completion time.
type fleet struct {
	*Manager
	Runtime   *infra.BackendRuntime
	Integrity *syncutil.AtomicConfig[config.IntegrityConfig]
}

// SetIntegrityConfig swaps the integrity settings the manager reads when it
// completes an upload.
func (f *fleet) SetIntegrityConfig(cfg *config.IntegrityConfig) { f.Integrity.Store(cfg) }

// newFleet builds a multipart Manager over the supplied backends. store is the
// wide metadata store; New narrows it to MultipartStores.
func newFleet(
	t *testing.T, store core.MetadataStore, backends map[string]backend.ObjectBackend, opts *fleetOpts,
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
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(names), nil)
	timeout := cmp.Or(opts.BackendTimeout, fleetTimeout)
	rt := infra.New(&infra.Config{
		Backends:        backends,
		Order:           names,
		BackendTimeout:  timeout,
		Usage:           usage,
		RoutingStrategy: config.RoutingPack,
		AdmissionSem:    opts.AdmissionSem,
	})
	rt.SetMetricsCollector(metrics.New(metrics.CollectorDeps{
		Store: store, Usage: usage, BackendNames: names,
	}))

	integrity := &syncutil.AtomicConfig[config.IntegrityConfig]{}
	mp := New(&Deps{
		Core:               rt,
		Coord:              writepath.New(rt, store, opts.PendingEnabled),
		Stores:             store,
		Encryptor:          opts.Encryptor,
		ObjectCache:        opts.ObjectCache,
		DEKCacheTTL:        fleetDEKTTL,
		IntegrityCfg:       integrity,
		EnforceMinPartSize: opts.EnforceMinPartSize,
	})
	t.Cleanup(mp.Close)
	return &fleet{Manager: mp, Runtime: rt, Integrity: integrity}
}

// newPermissiveStore returns a union store mock answering every read with an
// empty result, so a test states only the queries it asserts on.
func newPermissiveStore(t *testing.T) *storetest.MockMetadataStore {
	t.Helper()
	m := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(m)
	return m
}

// partsOf builds a completion manifest from bare part numbers, for the tests
// whose subject is assembly rather than manifest validation. An empty ETag
// skips the stored-ETag comparison, so these keep exercising the path they
// always did.
func partsOf(numbers ...int) []core.CompletePart {
	manifest := make([]core.CompletePart, len(numbers))
	for i, n := range numbers {
		manifest[i] = core.CompletePart{PartNumber: n}
	}
	return manifest
}
