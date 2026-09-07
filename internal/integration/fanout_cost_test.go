// -------------------------------------------------------------------------------
// Integration Tests - What Write Fan-Out Costs
//
// Author: Alex Freidah
//
// The case for placing an object's copies during the write is that the
// replicator's alternative reads every object back off a backend and pays that
// backend's egress to do it. That is an argument until it is a number, so this
// runs one workload both ways against real backends and reports what each
// charged: backend API calls, bytes read, bytes written.
//
// The assertions are on the shape of the difference rather than on exact
// figures, which move with object size and backend count. The logged table is
// the artifact worth reading.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
)

// fanoutCostObjects is the workload size. Large enough that per-write noise
// averages out, small enough that the suite stays quick.
const fanoutCostObjects = 40

// fanoutCostObjectSize is what each write carries. A size that survives being
// reported in bytes without becoming unreadable.
const fanoutCostObjectSize = 32 << 10

// fleetCost is what one workload charged the whole fleet.
type fleetCost struct {
	APICalls int64
	Egress   int64
	Ingress  int64
}

// String renders the row this test exists to produce.
func (c fleetCost) String() string {
	return fmt.Sprintf("api_calls=%d bytes_read=%d bytes_written=%d", c.APICalls, c.Egress, c.Ingress)
}

// readFleetCost totals the live counters across every backend in the fleet.
func readFleetCost(rt *infra.BackendRuntime, names []string) fleetCost {
	var total fleetCost
	for _, name := range names {
		u := readUsage(rt, name)
		total.APICalls += u.APICalls
		total.Egress += u.Egress
		total.Ingress += u.Ingress
	}
	return total
}

// sub returns what was charged between two readings.
func (c fleetCost) sub(prev fleetCost) fleetCost {
	return fleetCost{
		APICalls: c.APICalls - prev.APICalls,
		Egress:   c.Egress - prev.Egress,
		Ingress:  c.Ingress - prev.Ingress,
	}
}

// runFanoutWorkload writes the workload against a fleet built to spec, brings
// every object up to factor 2 - by whichever mechanism the spec implies - and
// returns what the fleet was charged.
func runFanoutWorkload(t *testing.T, spec harnessSpec, keyPrefix string) fleetCost {
	t.Helper()
	h := newHarness(t, spec)
	body := bytes.Repeat([]byte("fan-out cost"), fanoutCostObjectSize/12)

	names := make([]string, 0, len(spec.Backends))
	for _, b := range spec.Backends {
		names = append(names, b.Name)
	}

	before := readFleetCost(h.stack.Runtime, names)
	for i := range fanoutCostObjects {
		h.put(fmt.Sprintf("%s/obj-%03d", keyPrefix, i), body)
	}

	// Both modes are measured at the same end state: every object at factor.
	// Whatever the write did not place, the replicator makes, and its reads are
	// exactly the cost being compared. A pass per tick rather than a tight
	// poll, since each one is real work against the backends.
	testx.EventuallyInterval(t, 60*time.Second, 250*time.Millisecond, func() bool {
		h.replicate(2)
		return shortOfFactor(t, h, 2) == 0
	}, "objects never reached factor 2")

	return readFleetCost(h.stack.Runtime, names).sub(before)
}

// shortOfFactor counts the keys holding fewer copies than the factor, read
// from the ledger rather than from a worker's own report.
func shortOfFactor(t *testing.T, h *harness, factor int) int {
	t.Helper()
	var n int
	if err := h.db.QueryRow(
		`SELECT count(*) FROM (
		     SELECT object_key FROM object_locations GROUP BY object_key HAVING count(*) < $1
		 ) short`, factor).Scan(&n); err != nil {
		t.Fatalf("count under-replicated: %v", err)
	}
	return n
}

// TestInt_FanoutCost_RemovesTheReplicatorsRead is the measurement behind the
// feature. The same workload runs twice, and the fan-out run must reach the
// same replication factor without reading the objects back.
func TestInt_FanoutCost_RemovesTheReplicatorsRead(t *testing.T) {
	backends := []harnessBackend{
		{Name: "h-minio-1", Quota: 256 << 20},
		{Name: "h-minio-2", Quota: 256 << 20},
		{Name: "h-minio-3", Quota: 256 << 20},
	}

	replicated := runFanoutWorkload(t, harnessSpec{Backends: backends}, "cost-replicated")
	fanout := runFanoutWorkload(t, harnessSpec{Backends: backends, CopiesPerWrite: 2}, "cost-fanout")

	t.Logf("write-one-then-replicate: %s", replicated)
	t.Logf("fan-out:                  %s", fanout)
	t.Logf("saved: %d backend API calls, %d bytes read",
		replicated.APICalls-fanout.APICalls, replicated.Egress-fanout.Egress)

	// The read is the whole point: replication cannot make a copy without one,
	// and a write placing its own never needs it.
	if fanout.Egress >= replicated.Egress {
		t.Errorf("fan-out read %d bytes against %d for write-then-replicate; the read it removes is the feature",
			fanout.Egress, replicated.Egress)
	}
	if replicated.Egress == 0 {
		t.Fatal("the write-then-replicate run read nothing, so the comparison measured nothing")
	}

	// Fewer calls too: the same copies land either way, but replication needs a
	// GET per copy on top of the PUT.
	if fanout.APICalls >= replicated.APICalls {
		t.Errorf("fan-out made %d backend calls against %d for write-then-replicate",
			fanout.APICalls, replicated.APICalls)
	}

	// Both wrote the same bytes, because both ended at the same factor. A large
	// gap here would mean the runs are not comparable.
	if diff := fanout.Ingress - replicated.Ingress; diff > replicated.Ingress/10 || -diff > replicated.Ingress/10 {
		t.Errorf("the runs wrote %d and %d bytes; they are not the same workload",
			fanout.Ingress, replicated.Ingress)
	}
}
