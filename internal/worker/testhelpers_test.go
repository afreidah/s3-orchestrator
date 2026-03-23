package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// mockMetadataStore is a minimal stub for worker tests.
type mockMetadataStore struct {
	store.MetadataStore // embed to satisfy interface
	pendingCleanups     []store.CleanupItem
	completedIDs        []int64
	randomHashedObjects []store.ObjectLocation
	objectsWithoutHash  []store.ObjectLocation
	lastUpdatedHash     string
}

func (m *mockMetadataStore) GetPendingCleanups(_ context.Context, _ int) ([]store.CleanupItem, error) {
	return m.pendingCleanups, nil
}

func (m *mockMetadataStore) CompleteCleanupItem(_ context.Context, id int64) error {
	m.completedIDs = append(m.completedIDs, id)
	return nil
}

func (m *mockMetadataStore) RetryCleanupItem(_ context.Context, _ int64, _ time.Duration, _ string) error {
	return nil
}

func (m *mockMetadataStore) DecrementOrphanBytes(_ context.Context, _ string, _ int64) error {
	return nil
}

func (m *mockMetadataStore) CleanupQueueDepth(_ context.Context) (int64, error) {
	return 0, nil
}

func (m *mockMetadataStore) GetRandomHashedObjects(_ context.Context, _ int) ([]store.ObjectLocation, error) {
	return m.randomHashedObjects, nil
}

func (m *mockMetadataStore) GetObjectsWithoutHash(_ context.Context, limit, _ int) ([]store.ObjectLocation, error) {
	if limit > len(m.objectsWithoutHash) {
		return m.objectsWithoutHash, nil
	}
	return m.objectsWithoutHash[:limit], nil
}

func (m *mockMetadataStore) UpdateContentHash(_ context.Context, _, _, hash string) error {
	m.lastUpdatedHash = hash
	return nil
}

// newTestUsageTracker creates a UsageTracker with no limits for testing.
func newTestUsageTracker() *counter.UsageTracker {
	return counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
}
