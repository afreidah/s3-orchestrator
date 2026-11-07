// -------------------------------------------------------------------------------
// Bulk Delete Group Tests
//
// Author: Alex Freidah
//
// A bulk delete splits into transactions of a bounded size, so a batch cannot
// take more locks than the database has room for. A group that fails costs only
// its own keys, and the bytes of every group that committed still go.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

const (
	groupedKeys   = 250
	wantGroupSize = 100
)

// groupedDelete drives a bulk delete over groupedKeys keys held on one backend,
// failing the group at position failGroup when it is not negative.
type groupedDelete struct {
	keys    []string
	be      *backendtest.InMemory
	groups  [][]string
	settles int
	results []DeleteObjectResult
}

// runGroupedDelete seeds the keys and runs the delete against a store that
// records each transaction and each settle.
func runGroupedDelete(t *testing.T, failGroup int) *groupedDelete {
	t.Helper()
	ctx := context.Background()
	run := &groupedDelete{be: backendtest.NewInMemory()}
	for i := range groupedKeys {
		key := fmt.Sprintf("key-%03d", i)
		run.keys = append(run.keys, key)
		if _, err := run.be.PutObject(ctx, key, bytes.NewReader([]byte("x")), 1, "", nil); err != nil {
			t.Fatal(err)
		}
	}

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	objectsStubs(store)
	var nextID int64
	store.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, keys []string) ([]core.CleanupItem, error) {
			run.groups = append(run.groups, slices.Clone(keys))
			if len(run.groups)-1 == failGroup {
				return nil, errors.New("group failed")
			}
			rows := make([]core.CleanupItem, len(keys))
			for i, key := range keys {
				nextID++
				rows[i] = core.CleanupItem{
					ID: nextID, BackendName: "b1", ObjectKey: key, StorageKey: key, Reason: core.CleanupReasonDeleted, SizeBytes: 1,
				}
			}
			return rows, nil
		}).AnyTimes()
	store.EXPECT().CompleteCleanupItems(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, items []core.CleanupItem) ([]int64, error) {
			run.settles++
			ids := make([]int64, len(items))
			for i := range items {
				ids[i] = items[i].ID
			}
			return ids, nil
		}).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": run.be}, nil)
	run.results = mgr.DeleteObjects(ctx, run.keys)
	return run
}

// TestDeleteObjects_BoundsEachTransaction verifies a bulk delete reaches the
// store in groups no larger than the limit, covering every key once.
func TestDeleteObjects_BoundsEachTransaction(t *testing.T) {
	t.Parallel()
	run := runGroupedDelete(t, -1)
	if len(run.groups) != 3 {
		t.Fatalf("transactions = %d, want 3", len(run.groups))
	}
	var seen []string
	for _, group := range run.groups {
		if len(group) > wantGroupSize {
			t.Errorf("group holds %d keys, want at most %d", len(group), wantGroupSize)
		}
		seen = append(seen, group...)
	}
	if !slices.Equal(seen, run.keys) {
		t.Errorf("groups covered %d keys, want each of %d once", len(seen), len(run.keys))
	}
}

// TestDeleteObjects_FailedGroupFailsOnlyItsKeys verifies a failed group reports
// its own keys and keeps their bytes, the other groups' keys succeed, and their
// bytes are settled together.
func TestDeleteObjects_FailedGroupFailsOnlyItsKeys(t *testing.T) {
	t.Parallel()
	const failedGroup = 1
	run := runGroupedDelete(t, failedGroup)
	if len(run.groups) <= failedGroup {
		t.Fatalf("transactions = %d, want the batch split so one group can fail", len(run.groups))
	}
	failed := run.groups[failedGroup]
	for _, res := range run.results {
		inFailed := slices.Contains(failed, res.Key)
		if inFailed != (res.Err != nil) {
			t.Errorf("%s: err = %v, in failed group = %v", res.Key, res.Err, inFailed)
		}
		if inFailed != run.be.HasCopyOf(res.Key) {
			t.Errorf("%s: bytes present = %v, in failed group = %v", res.Key, run.be.HasCopyOf(res.Key), inFailed)
		}
	}
	if run.settles != 1 {
		t.Errorf("settles = %d, want the committed groups settled once", run.settles)
	}
}
