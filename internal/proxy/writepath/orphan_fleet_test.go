// -------------------------------------------------------------------------------
// Orphan Bytes Tracking Tests
//
// Author: Alex Freidah
//
// Validates the orphan bytes lifecycle: enqueue increments, successful cleanup
// decrements, exhausted cleanup preserves, displaced copies on overwrite are
// enqueued with correct sizes, and replicator capacity checks account for
// orphan bytes.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"go.uber.org/mock/gomock"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// orphanCalls accumulates the store interactions these tests assert on: the
// cleanup rows the coordinator enqueued and the orphan-byte adjustments it
// charged alongside them.
type orphanCalls struct {
	mu        sync.Mutex
	enqueue   []core.CleanupItem
	increment []orphanBytesEntry
}

// orphanBytesEntry is one orphan-byte adjustment: which backend was charged
// or credited, and for how many bytes.
type orphanBytesEntry struct {
	backendName string
	sizeBytes   int64
}

// stubOrphanEnqueue captures EnqueueCleanup args.
func stubOrphanEnqueue(c *orphanCalls, err error) func(context.Context, string, string, string, int64) error {
	return func(_ context.Context, backend, key, reason string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.enqueue = append(c.enqueue, core.CleanupItem{
			BackendName: backend, ObjectKey: key, Reason: reason, SizeBytes: size,
		})
		return err
	}
}

// stubOrphanIncrement captures IncrementOrphanBytes args.
func stubOrphanIncrement(c *orphanCalls, err error) func(context.Context, string, int64) error {
	return func(_ context.Context, backend string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.increment = append(c.increment, orphanBytesEntry{backendName: backend, sizeBytes: size})
		return err
	}
}

// TestEnqueueCleanup_IncrementsOrphanBytes asserts a non-zero enqueue
// drives one IncrementOrphanBytes call.
func TestEnqueueCleanup_IncrementsOrphanBytes(t *testing.T) {
	t.Parallel()
	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	coord, _ := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)
	coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "delete_failed", 4096)

	if len(c.increment) != 1 {
		t.Fatalf("expected 1 IncrementOrphanBytes call, got %d", len(c.increment))
	}
	if c.increment[0].backendName != "b1" || c.increment[0].sizeBytes != 4096 {
		t.Errorf("unexpected IncrementOrphanBytes call: %+v", c.increment[0])
	}
}

// TestEnqueueCleanup_ZeroSize_SkipsOrphanIncrement asserts a zero-size
// enqueue doesn't call IncrementOrphanBytes.
func TestEnqueueCleanup_ZeroSize_SkipsOrphanIncrement(t *testing.T) {
	t.Parallel()
	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	coord, _ := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)
	coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "delete_failed", 0)

	if len(c.increment) != 0 {
		t.Errorf("expected 0 IncrementOrphanBytes calls for zero-size, got %d", len(c.increment))
	}
}

// TestEnqueueCleanup_EnqueueFails_SkipsOrphanIncrement asserts no
// orphan-bytes increment when enqueue itself fails.
func TestEnqueueCleanup_EnqueueFails_SkipsOrphanIncrement(t *testing.T) {
	t.Parallel()
	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, errors.New("db down"))).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	coord, _ := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)
	coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "delete_failed", 4096)

	if len(c.increment) != 0 {
		t.Errorf("expected 0 IncrementOrphanBytes calls when enqueue fails, got %d", len(c.increment))
	}
}

// TestEnqueueCleanup_IncrementOrphanBytesFails asserts a best-effort
// increment failure does not abort the enqueue.
func TestEnqueueCleanup_IncrementOrphanBytesFails(t *testing.T) {
	t.Parallel()
	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, errors.New("db error"))).AnyTimes()
	storetest.Permissive(store)

	coord, _ := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)
	coord.EnqueueCleanup(context.Background(), "b1", "key", "reason", 1024)

	if len(c.enqueue) != 1 {
		t.Errorf("expected 1 enqueue call, got %d", len(c.enqueue))
	}
	if len(c.increment) != 1 {
		t.Errorf("expected 1 IncrementOrphanBytes call (even though it failed), got %d", len(c.increment))
	}
}

// cleanupCalls records the cleanup rows the coordinator enqueued, for the
// tests that assert on the enqueue itself rather than on the orphan-byte
// accounting alongside it.
type cleanupCalls struct {
	mu      sync.Mutex
	enqueue []core.CleanupItem
}

// stubEnqueue captures EnqueueCleanup calls into c.enqueue and returns
// the supplied error so tests can drive both happy and DB-outage paths.
func stubEnqueue(c *cleanupCalls, err error) func(context.Context, string, string, string, int64) error {
	return func(_ context.Context, backend, key, reason string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.enqueue = append(c.enqueue, core.CleanupItem{
			BackendName: backend, ObjectKey: key, Reason: reason, SizeBytes: size,
		})
		return err
	}
}

// TestEnqueueCleanup_Success captures a single enqueue call.
func TestEnqueueCleanup_Success(t *testing.T) {
	t.Parallel()
	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, nil)).
		AnyTimes()
	storetest.Permissive(store)

	coord, _ := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "orphan_put", 1024)

	if len(calls.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(calls.enqueue))
	}
	c := calls.enqueue[0]
	if c.BackendName != "b1" || c.ObjectKey != "orphan.txt" || c.Reason != "orphan_put" {
		t.Errorf("unexpected call: %+v", c)
	}
}

// TestEnqueueCleanup_DBError_LogsOnly asserts the call is recorded even
// when the store returns an error.
func TestEnqueueCleanup_DBError_LogsOnly(t *testing.T) {
	t.Parallel()
	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, errors.New("db down"))).
		AnyTimes()
	storetest.Permissive(store)

	coord, _ := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)
	coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "orphan_put", 1024)

	if len(calls.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(calls.enqueue))
	}
}

// TestEnqueueCleanup_EnqueueFailure_RecordsMetricAndAudit pins the
// visibility contract for #805. When the cleanup_queue row itself
// fails to persist (DB outage after a successful backend write), the
// system must:
//
//   - increment s3o_cleanup_enqueue_failures_total{stage="enqueue"}
//     so operators can alert on untracked orphan risk
//   - emit a storage.OrphanEnqueueFailed audit event so operators
//     can pivot from the metric to the exact backend/key/size
//
// Without these signals, the orphan would be silent (logged-only).
func TestEnqueueCleanup_EnqueueFailure_RecordsMetricAndAudit(t *testing.T) {
	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, errors.New("db down"))).
		AnyTimes()
	storetest.Permissive(store)
	coord, _ := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	var capturedEvents []string
	audit.SetOnEvent(func(event string) {
		capturedEvents = append(capturedEvents, event)
	})
	t.Cleanup(func() { audit.SetOnEvent(nil) })

	before := promtest.ToFloat64(telemetry.CleanupEnqueueFailuresTotal.WithLabelValues("b1", "orphan_put", "enqueue"))
	coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "orphan_put", 1024)
	after := promtest.ToFloat64(telemetry.CleanupEnqueueFailuresTotal.WithLabelValues("b1", "orphan_put", "enqueue"))

	if after-before != 1 {
		t.Errorf("enqueue-failure counter delta = %v, want 1", after-before)
	}
	found := slices.Contains(capturedEvents, "storage.OrphanEnqueueFailed")
	if !found {
		t.Errorf("expected storage.OrphanEnqueueFailed audit event, got %v", capturedEvents)
	}
}

// TestEnqueueCleanup_OrphanBytesFailure_RecordsMetricAndAudit covers
// the secondary failure path: the cleanup_queue row persisted but the
// orphan_bytes counter increment failed. Less severe than an enqueue
// failure (the cleanup worker will still retry the delete) but quota
// accounting drifts until reconciliation. The metric labels stage
// "orphan_bytes" so dashboards can distinguish the two failure
// shapes.
func TestEnqueueCleanup_OrphanBytesFailure_RecordsMetricAndAudit(t *testing.T) {
	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(errors.New("db down")).AnyTimes()
	storetest.Permissive(store)
	coord, _ := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	var capturedEvents []string
	audit.SetOnEvent(func(event string) {
		capturedEvents = append(capturedEvents, event)
	})
	t.Cleanup(func() { audit.SetOnEvent(nil) })

	before := promtest.ToFloat64(telemetry.CleanupEnqueueFailuresTotal.WithLabelValues("b1", "orphan_put", "orphan_bytes"))
	coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "orphan_put", 1024)
	after := promtest.ToFloat64(telemetry.CleanupEnqueueFailuresTotal.WithLabelValues("b1", "orphan_put", "orphan_bytes"))

	if after-before != 1 {
		t.Errorf("orphan-bytes-failure counter delta = %v, want 1", after-before)
	}
	found := slices.Contains(capturedEvents, "storage.OrphanEnqueueFailed")
	if !found {
		t.Errorf("expected storage.OrphanEnqueueFailed audit event, got %v", capturedEvents)
	}
}
