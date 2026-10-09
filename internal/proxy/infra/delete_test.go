// -------------------------------------------------------------------------------
// Backend Runtime Tests - Deletes
//
// Author: Alex Freidah
//
// Covers DeleteMany's choice between a batch and single deletes, the per-backend
// memory of a declined batch, how a failed batch is reported, and how many API
// calls each path charges.
// -------------------------------------------------------------------------------

package infra

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
)

// newDeleteRuntime builds a runtime over one in-memory backend named b1 holding
// objs, and returns the counters its API calls are charged to.
func newDeleteRuntime(t *testing.T, mem *backendtest.InMemory, objs ...string) (*BackendRuntime, *counter.LocalCounterBackend) {
	t.Helper()
	for _, k := range objs {
		mem.Objects[k] = backendtest.Object{}
	}
	counters := counter.NewLocalCounterBackend([]string{"b1"})
	return New(&Config{
		Backends:        map[string]backend.ObjectBackend{"b1": mem},
		Order:           []string{"b1"},
		BackendTimeout:  time.Second,
		Usage:           counter.NewUsageTracker(counters, nil),
		RoutingStrategy: config.RoutingPack,
		Log:             slog.Default(),
	}), counters
}

// TestDeleteMany_BatchesWhereSupported verifies keys go out as one batch
// charged as one call.
func TestDeleteMany_BatchesWhereSupported(t *testing.T) {
	t.Parallel()
	mem := backendtest.NewInMemory()
	mem.BatchDeleteEnabled = true
	rt, counters := newDeleteRuntime(t, mem, "a", "b", "c")

	if failed := rt.DeleteMany(t.Context(), "b1", mem, []string{"a", "b", "c"}); len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	if mem.BatchDeleteCalls != 1 || len(mem.Objects) != 0 {
		t.Errorf("batch calls = %d, objects left = %d; want 1 and 0", mem.BatchDeleteCalls, len(mem.Objects))
	}
	if got := counters.Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("API calls charged = %d, want 1", got)
	}
}

// TestDeleteMany_FallsBackAndRemembers verifies a backend that declines batches
// gets single deletes, each charged, and is not asked to batch again.
func TestDeleteMany_FallsBackAndRemembers(t *testing.T) {
	t.Parallel()
	mem := backendtest.NewInMemory()
	rt, counters := newDeleteRuntime(t, mem, "a", "b", "c", "d")

	if failed := rt.DeleteMany(t.Context(), "b1", mem, []string{"a", "b"}); len(failed) != 0 {
		t.Fatalf("first call failed = %v, want none", failed)
	}
	mem.BatchDeleteEnabled = true // a batch would now succeed, but the runtime remembers the refusal
	if failed := rt.DeleteMany(t.Context(), "b1", mem, []string{"c", "d"}); len(failed) != 0 {
		t.Fatalf("second call failed = %v, want none", failed)
	}
	if mem.BatchDeleteCalls != 0 || len(mem.Objects) != 0 {
		t.Errorf("batch calls = %d, objects left = %d; want 0 and 0", mem.BatchDeleteCalls, len(mem.Objects))
	}
	if got := counters.Load("b1", counter.FieldAPIRequests); got != 4 {
		t.Errorf("API calls charged = %d, want one per single delete", got)
	}
}

// TestDeleteMany_ReportsFailures verifies per-key failures come back from a
// batch, and a batch that fails outright fails every key.
func TestDeleteMany_ReportsFailures(t *testing.T) {
	t.Parallel()
	mem := backendtest.NewInMemory()
	mem.BatchDeleteEnabled = true
	mem.BatchDeleteKeyErrs = map[string]error{"b": errors.New("denied")}
	rt, _ := newDeleteRuntime(t, mem, "a", "b")

	if failed := rt.DeleteMany(t.Context(), "b1", mem, []string{"a", "b"}); len(failed) != 1 || failed["b"] == nil {
		t.Errorf("per-key: failed = %v, want only b", failed)
	}

	mem.DeleteErr = errors.New("connection reset")
	if failed := rt.DeleteMany(t.Context(), "b1", mem, []string{"a", "b"}); len(failed) != 2 {
		t.Errorf("whole request: failed = %v, want both keys", failed)
	}
}

// TestDeleteMany_SingleKeyDeletesDirectly verifies one key is not sent as a
// batch.
func TestDeleteMany_SingleKeyDeletesDirectly(t *testing.T) {
	t.Parallel()
	mem := backendtest.NewInMemory()
	mem.BatchDeleteEnabled = true
	rt, _ := newDeleteRuntime(t, mem, "a")

	if failed := rt.DeleteMany(t.Context(), "b1", mem, []string{"a"}); len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	if mem.BatchDeleteCalls != 0 || len(mem.Objects) != 0 {
		t.Errorf("batch calls = %d, objects left = %d; want 0 and 0", mem.BatchDeleteCalls, len(mem.Objects))
	}
}
