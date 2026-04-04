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
	underReplicated     []store.ObjectLocation
	overReplicated      []store.ObjectLocation
	overReplicatedCount int64
	quotaStats          map[string]store.QuotaStat
	recordReplicaOK     bool
	replicaRecorded     int
	removedCopies       int
	objectsByBackend    map[string][]store.ObjectLocation
	moveSize            int64
	staleDeleted        int
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

func (m *mockMetadataStore) GetUnderReplicatedObjects(_ context.Context, _, _ int) ([]store.ObjectLocation, error) {
	return m.underReplicated, nil
}

func (m *mockMetadataStore) GetUnderReplicatedObjectsExcluding(_ context.Context, _, _ int, _ []string) ([]store.ObjectLocation, error) {
	return m.underReplicated, nil
}

func (m *mockMetadataStore) GetQuotaStats(_ context.Context) (map[string]store.QuotaStat, error) {
	return m.quotaStats, nil
}

func (m *mockMetadataStore) RecordReplica(_ context.Context, _, _, _ string, _ int64) (bool, error) {
	m.replicaRecorded++
	return m.recordReplicaOK, nil
}

func (m *mockMetadataStore) GetOverReplicatedObjects(_ context.Context, _, _ int) ([]store.ObjectLocation, error) {
	return m.overReplicated, nil
}

func (m *mockMetadataStore) CountOverReplicatedObjects(_ context.Context, _ int) (int64, error) {
	return m.overReplicatedCount, nil
}

func (m *mockMetadataStore) RemoveExcessCopy(_ context.Context, _, _ string, _ int64) error {
	m.removedCopies++
	return nil
}

func (m *mockMetadataStore) ListObjectsByBackend(_ context.Context, name string, _ int) ([]store.ObjectLocation, error) {
	return m.objectsByBackend[name], nil
}

func (m *mockMetadataStore) MoveObjectLocation(_ context.Context, _, _, _ string) (int64, error) {
	return m.moveSize, nil
}

func (m *mockMetadataStore) FlushUsageDeltas(_ context.Context, _, _ string, _, _, _ int64) error {
	return nil
}

func (m *mockMetadataStore) DeleteObjectLocation(_ context.Context, _, _ string) error {
	m.staleDeleted++
	return nil
}
