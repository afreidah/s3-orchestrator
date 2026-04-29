// -------------------------------------------------------------------------------
// Drain Helper Tests - migrateBackendObjects, abortDrainWithError, finalizeDrain
//
// Author: Alex Freidah
//
// Direct unit coverage of the helpers extracted from runDrain. The end-to-end
// drain flow is exercised by TestStartDrain_* and TestRunDrain_* in
// manager_drain_test.go; these tests pin each helper's individual contract.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"testing"

	st "github.com/afreidah/s3-orchestrator/internal/store"
)

// newDrainState builds a drainState matching what StartDrain would create.
func newDrainState() *drainState {
	_, cancel := context.WithCancel(context.Background())
	return &drainState{
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

// -------------------------------------------------------------------------
// abortDrainWithError
// -------------------------------------------------------------------------

func TestAbortDrainWithError_ClearsStateAndRecordsError(t *testing.T) {
	t.Parallel()
	mgr := newDrainTestManager(&mockStore{}, map[string]*mockBackend{"b1": newMockBackend()})
	state := newDrainState()
	mgr.draining.Store("b1", state)

	wantErr := errors.New("simulated drain failure")
	mgr.DrainManager.abortDrainWithError("b1", state, wantErr)

	if got := state.getErr(); !errors.Is(got, wantErr) {
		t.Errorf("state.getErr() = %v, want %v", got, wantErr)
	}
	if _, ok := mgr.draining.Load("b1"); ok {
		t.Error("draining entry should have been removed")
	}
}

// -------------------------------------------------------------------------
// migrateBackendObjects
// -------------------------------------------------------------------------

func TestMigrateBackendObjects_EmptyBackendReturnsNil(t *testing.T) {
	t.Parallel()
	store := &mockStore{listObjectsByBackendResp: nil}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	state := newDrainState()
	srcBackend, _ := mgr.DrainManager.getBackend("b1")
	if err := mgr.DrainManager.migrateBackendObjects(context.Background(), "b1", state, srcBackend); err != nil {
		t.Fatalf("migrateBackendObjects: %v", err)
	}
	if state.moved.Load() != 0 {
		t.Errorf("state.moved = %d, want 0 (no objects to migrate)", state.moved.Load())
	}
}

func TestMigrateBackendObjects_PropagatesStoreError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("db connection lost")
	store := &mockStore{listObjectsByBackendErr: wantErr}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	state := newDrainState()
	srcBackend, _ := mgr.DrainManager.getBackend("b1")
	err := mgr.DrainManager.migrateBackendObjects(context.Background(), "b1", state, srcBackend)
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want %v", err, wantErr)
	}
}

func TestMigrateBackendObjects_PropagatesCancellation(t *testing.T) {
	t.Parallel()
	// Provide a non-empty page so the loop reaches the inner cancellation
	// check rather than exiting on empty.
	store := &mockStore{
		listObjectsByBackendResp: []st.ObjectLocation{
			{ObjectKey: "k1", BackendName: "b1", SizeBytes: 1},
		},
	}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	state := newDrainState()
	srcBackend, _ := mgr.DrainManager.getBackend("b1")
	err := mgr.DrainManager.migrateBackendObjects(ctx, "b1", state, srcBackend)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestMigrateBackendObjects_IncrementsMovedOnSuccess(t *testing.T) {
	t.Parallel()
	// Two source rows on b1; GetAllObjectLocations returns a row on b2 so
	// DrainOneObject takes the "replica exists elsewhere" fast-path and
	// reports success without needing a full stream-copy. The mockStore
	// returns the same getAllLocationsResp for both k1 and k2; the only
	// row attribute DrainOneObject inspects is BackendName, so this is
	// sufficient to drive the success path for both objects.
	store := &mockStore{
		listObjectsByBackendPages: [][]st.ObjectLocation{
			{
				{ObjectKey: "k1", BackendName: "b1", SizeBytes: 4},
				{ObjectKey: "k2", BackendName: "b1", SizeBytes: 5},
			},
			nil, // second page empty → loop exits
		},
		getAllLocationsResp: []st.ObjectLocation{
			{BackendName: "b1"},
			{BackendName: "b2"}, // replica elsewhere → fast-path delete
		},
	}
	mgr := newDrainTestManager(store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
	})

	state := newDrainState()
	srcBackend, _ := mgr.DrainManager.getBackend("b1")
	if err := mgr.DrainManager.migrateBackendObjects(context.Background(), "b1", state, srcBackend); err != nil {
		t.Fatalf("migrateBackendObjects: %v", err)
	}
	if got := state.moved.Load(); got != 2 {
		t.Errorf("state.moved = %d, want 2 (both objects migrated via replica fast-path)", got)
	}
}

// -------------------------------------------------------------------------
// finalizeDrain
// -------------------------------------------------------------------------

func TestFinalizeDrain_HappyPathLeavesNoError(t *testing.T) {
	t.Parallel()
	mgr := newDrainTestManager(&mockStore{}, map[string]*mockBackend{"b1": newMockBackend()})
	state := newDrainState()

	mgr.DrainManager.finalizeDrain(context.Background(), "b1", state)

	if got := state.getErr(); got != nil {
		t.Errorf("state.getErr() = %v, want nil", got)
	}
}

func TestFinalizeDrain_DeleteBackendDataErrorRecordedOnState(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("db write failed")
	store := &mockStore{deleteBackendDataErr: wantErr}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	state := newDrainState()
	mgr.DrainManager.finalizeDrain(context.Background(), "b1", state)

	if got := state.getErr(); !errors.Is(got, wantErr) {
		t.Errorf("state.getErr() = %v, want %v", got, wantErr)
	}
}
