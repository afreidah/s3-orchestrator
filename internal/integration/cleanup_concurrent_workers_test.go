// -------------------------------------------------------------------------------
// Integration Tests - Cleanup Queue Concurrent Workers
//
// Author: Alex Freidah
//
// Pins the worker-level invariant that multiple CleanupWorker instances
// processing the same queue concurrently never double-process a row.
// The store layer's FOR UPDATE SKIP LOCKED is already covered by
// TestStoreInt_ClaimPendingCleanups_ConcurrentDisjoint; this test
// wraps the full claim -> process -> complete loop with multiple
// worker instances racing on the same queue.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newAuxCleanupWorker builds an additional CleanupWorker instance that
// shares testManager and testStore but carries a distinct instanceID.
// Lets a test spin up N workers all racing on the same queue.
func newAuxCleanupWorker(instanceID string) *worker.CleanupWorker {
	return worker.NewCleanupWorker(worker.CleanupWorkerDeps{Ops: testStack.Runtime, Store: testStore, Concurrency: 10, InstanceID: instanceID, ClaimGracePeriod: 5 * time.Minute})
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestInt_CleanupQueue_ConcurrentWorkersProcessExactlyOnce enqueues N
// items, spawns K worker instances with distinct instanceIDs, and
// drives them in parallel. The full row count must be processed
// exactly once across the workers: any double-processing would show
// up as resolved-sum > N, and any lost row would leave the queue
// non-empty.
func TestInt_CleanupQueue_ConcurrentWorkersProcessExactlyOnce(t *testing.T) {
	resetState(t)

	const (
		rows    = 40
		workers = 4
	)
	backend := pickBackend(t)
	ctx := context.Background()

	for i := range rows {
		key := internalKey(fmt.Sprintf("concurrent-cleanup-%d-%s", i, uniqueKey(t, "race")))
		if err := testStore.EnqueueCleanup(ctx, backend, key, "concurrency_test", 0); err != nil {
			t.Fatalf("EnqueueCleanup %d: %v", i, err)
		}
	}

	// Bring up K-1 aux workers; the bundled testWorkers.CleanupWorker
	// is the Kth, so all share the same store but distinct IDs.
	pool := []*worker.CleanupWorker{testWorkers.CleanupWorker}
	for i := 1; i < workers; i++ {
		pool = append(pool, newAuxCleanupWorker(fmt.Sprintf("race-worker-%d", i)))
	}

	var resolvedTotal, failedTotal atomic.Int64
	var wg sync.WaitGroup
	for _, w := range pool {
		wg.Go(func() {
			// One pass is enough: each ProcessCleanupQueue call claims a
			// batch (up to 50 with the default), and 40 rows fits in a
			// single sweep. SKIP LOCKED makes the per-call disjointness
			// the load-bearing assertion here, not the loop.
			cleanSum := w.ProcessCleanupQueue(ctx)
			resolved, failed := cleanSum.Succeeded, cleanSum.Failed
			resolvedTotal.Add(int64(resolved))
			failedTotal.Add(int64(failed))
		})
	}
	wg.Wait()

	if got := resolvedTotal.Load(); got != rows {
		t.Errorf("resolved across workers = %d, want %d (over=double-process, under=lost row)", got, rows)
	}
	if got := failedTotal.Load(); got != 0 {
		t.Errorf("failed across workers = %d, want 0", got)
	}
	if remaining := queryCleanupQueueCount(t, backend); remaining != 0 {
		t.Errorf("cleanup_queue remaining = %d, want 0", remaining)
	}
}

// pickBackend returns the first configured test backend so the test
// can enqueue cleanup rows against a real backend name. Mirrors what
// queryObjectBackend assumes about the helper-managed fixture.
func pickBackend(t *testing.T) string {
	t.Helper()
	if len(testBackendOrder) == 0 {
		t.Fatal("no backends configured in test fixture")
	}
	return testBackendOrder[0]
}
