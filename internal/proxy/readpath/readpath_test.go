// -------------------------------------------------------------------------------
// Read Failover Tests - Caller-Supplied Location Lookup
//
// Author: Alex Freidah
//
// Read takes the caller's lookup result rather than querying the store, so
// these drive each outcome of that lookup directly: rows to walk, a missing
// key, a database outage, and any other store error.
// -------------------------------------------------------------------------------

package readpath

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/counter/countertest"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// readTestKey is the object key every Read test asks for.
const readTestKey = "k"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// readTestRuntime serves a fixed backend set and discards accounting.
type readTestRuntime struct {
	backends map[string]backend.ObjectBackend
	order    []string
	acct     *accounting.Recorder
}

// mapLocationCache is a plain map standing in for the object package's
// location cache.
type mapLocationCache map[string]string

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

func (r *readTestRuntime) GetBackend(name string) (backend.ObjectBackend, error) {
	be, ok := r.backends[name]
	if !ok {
		return nil, fmt.Errorf("backend %s not found", name)
	}
	return be, nil
}

func (r *readTestRuntime) BackendOrder() []string     { return r.order }
func (r *readTestRuntime) Acct() *accounting.Recorder { return r.acct }

func (c mapLocationCache) Get(key string) (string, bool) {
	v, ok := c[key]
	return v, ok
}

func (c mapLocationCache) Set(key, backendName string) { c[key] = backendName }

// newReadTestFailover builds a Failover over in-memory backends named a and b,
// returning the location cache so degraded-mode tests can inspect the winner.
func newReadTestFailover(degraded bool) (*Failover, mapLocationCache) {
	return newReadTestFailoverOver(degraded, nil)
}

// newReadTestFailoverOver is newReadTestFailover with the backends' pressure
// judged by pressure, or by nothing when it is nil.
func newReadTestFailoverOver(degraded bool, pressure *counter.PressureTracker) (*Failover, mapLocationCache) {
	rt := &readTestRuntime{
		backends: map[string]backend.ObjectBackend{"a": backendtest.NewInMemory(), "b": backendtest.NewInMemory()},
		order:    []string{"a", "b"},
		acct:     accounting.New(nil, pressure, func(_, _ string, _ time.Time, _ error) {}),
	}
	cache := mapLocationCache{}
	return New(&FailoverDeps{Core: rt, Cache: cache, DegradedReadsEnabled: degraded}), cache
}

// succeedOn returns a probe that wins on the named backend and fails on every
// other one, recording each location it was handed.
func succeedOn(winner string, seen *[]*core.ObjectLocation) Probe[string] {
	return func(_ context.Context, name string, loc *core.ObjectLocation, _ backend.ObjectBackend) (ProbeResult[string], error) {
		*seen = append(*seen, loc)
		if name != winner {
			return ProbeResult[string]{}, errors.New(name + " failed")
		}
		return ProbeResult[string]{Value: name, Size: 1, Cleanup: NoopCleanup}, nil
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestRead_WalksTheCallersLocations verifies Read tries the rows it was handed
// in order and passes each probe its own row, failing over to the next copy
// when the first one errors.
func TestRead_WalksTheCallersLocations(t *testing.T) {
	t.Parallel()
	f, _ := newReadTestFailover(true)
	locs := []core.ObjectLocation{{ObjectKey: readTestKey, BackendName: "a"}, {ObjectKey: readTestKey, BackendName: "b"}}
	var seen []*core.ObjectLocation

	got, name, err := f.Read(context.Background(), "GetObject", readTestKey, locs, nil, succeedOn("b", &seen))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "b" || name != "b" {
		t.Errorf("winner = %q/%q, want b", got, name)
	}
	if len(seen) != 2 || seen[0] != &locs[0] || seen[1] != &locs[1] {
		t.Errorf("probes saw %v, want each caller row in order", seen)
	}
}

// TestRead_PressuredBackendIsTriedLast verifies a copy on a backend under
// pressure is tried after the others, both when walking the caller's rows and
// when broadcasting, even past a cached winner on that backend.
func TestRead_PressuredBackendIsTriedLast(t *testing.T) {
	t.Parallel()
	f, cache := newReadTestFailoverOver(true, countertest.PressureTracker(t, []string{"a", "b"}, "a"))
	cache["k"] = "a"
	locs := []core.ObjectLocation{{ObjectKey: readTestKey, BackendName: "a"}, {ObjectKey: readTestKey, BackendName: "b"}}
	tried := func(names *[]string) Probe[string] {
		return func(_ context.Context, name string, _ *core.ObjectLocation, _ backend.ObjectBackend) (ProbeResult[string], error) {
			*names = append(*names, name)
			return ProbeResult[string]{Value: name, Cleanup: NoopCleanup}, nil
		}
	}

	var walked, broadcast []string
	if _, name, err := f.Read(context.Background(), "GetObject", readTestKey, locs, nil, tried(&walked)); err != nil || name != "b" {
		t.Errorf("walk won on %q, %v; want b", name, err)
	}
	if _, name, err := f.Read(context.Background(), "GetObject", readTestKey, nil, core.ErrDBUnavailable, tried(&broadcast)); err != nil || name != "b" {
		t.Errorf("broadcast won on %q, %v; want b", name, err)
	}
	if len(walked) != 1 || len(broadcast) != 1 {
		t.Errorf("tried %v walking and %v broadcasting, want only b each time", walked, broadcast)
	}
}

// TestRead_NotFoundSkipsEveryBackend verifies a lookup that found no object is
// returned as-is without probing any backend.
func TestRead_NotFoundSkipsEveryBackend(t *testing.T) {
	t.Parallel()
	f, _ := newReadTestFailover(true)
	var seen []*core.ObjectLocation

	_, _, err := f.Read(context.Background(), "GetObject", readTestKey, nil, core.ErrObjectNotFound, succeedOn("a", &seen))
	if !errors.Is(err, core.ErrObjectNotFound) {
		t.Errorf("error = %v, want ErrObjectNotFound", err)
	}
	if len(seen) != 0 {
		t.Errorf("probed %d backends, want none", len(seen))
	}
}

// TestRead_DBUnavailableBroadcasts verifies a database outage falls back to
// probing every backend with no row and remembers the winner for the next
// degraded read.
func TestRead_DBUnavailableBroadcasts(t *testing.T) {
	t.Parallel()
	f, cache := newReadTestFailover(true)
	var seen []*core.ObjectLocation

	got, name, err := f.Read(context.Background(), "GetObject", readTestKey, nil, core.ErrDBUnavailable, succeedOn("b", &seen))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "b" || name != "b" {
		t.Errorf("winner = %q/%q, want b", got, name)
	}
	for _, loc := range seen {
		if loc != nil {
			t.Errorf("degraded probe got row %+v, want nil", loc)
		}
	}
	if cache[readTestKey] != "b" {
		t.Errorf("cached winner = %q, want b", cache[readTestKey])
	}
}

// TestRead_AbandonedBroadcastReturnsTheContextError verifies an abandoned
// degraded read returns the context's error and counts no mixed outcome.
func TestRead_AbandonedBroadcastReturnsTheContextError(t *testing.T) {
	t.Parallel()
	const op = "abandoned-read-test"
	f, _ := newReadTestFailover(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := func(ctx context.Context, name string, _ *core.ObjectLocation, be backend.ObjectBackend) (ProbeResult[string], error) {
		if name == "b" {
			cancel()
			return ProbeResult[string]{}, ctx.Err()
		}
		_, err := be.HeadObject(ctx, readTestKey)
		return ProbeResult[string]{}, err
	}
	before := testutil.ToFloat64(telemetry.DegradedBroadcastMixedOutcomesTotal.WithLabelValues(op))

	_, _, err := f.Read(ctx, op, readTestKey, nil, core.ErrDBUnavailable, probe)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "all backends failed") {
		t.Errorf("error = %v, want the context's error alone", err)
	}
	if after := testutil.ToFloat64(telemetry.DegradedBroadcastMixedOutcomesTotal.WithLabelValues(op)); after != before {
		t.Errorf("mixed outcomes counted %v times, want none", after-before)
	}
}

// TestRead_DBUnavailableWithDegradedReadsOff verifies the operator switch
// fails a degraded read fast instead of broadcasting.
func TestRead_DBUnavailableWithDegradedReadsOff(t *testing.T) {
	t.Parallel()
	f, _ := newReadTestFailover(false)
	var seen []*core.ObjectLocation

	_, _, err := f.Read(context.Background(), "GetObject", readTestKey, nil, core.ErrDBUnavailable, succeedOn("a", &seen))
	if !errors.Is(err, core.ErrServiceUnavailable) {
		t.Errorf("error = %v, want ErrServiceUnavailable", err)
	}
	if len(seen) != 0 {
		t.Errorf("probed %d backends, want none", len(seen))
	}
}

// TestRead_OtherLookupErrorIsWrapped verifies any other lookup failure
// surfaces wrapped, with the cause intact, and no backend is probed.
func TestRead_OtherLookupErrorIsWrapped(t *testing.T) {
	t.Parallel()
	f, _ := newReadTestFailover(true)
	sentinel := errors.New("query failed")
	var seen []*core.ObjectLocation

	_, _, err := f.Read(context.Background(), "GetObject", readTestKey, nil, sentinel, succeedOn("a", &seen))
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the lookup error in its chain", err)
	}
	if !strings.Contains(err.Error(), "failed to find object location") {
		t.Errorf("error = %v, want the lookup context", err)
	}
	if len(seen) != 0 {
		t.Errorf("probed %d backends, want none", len(seen))
	}
}
