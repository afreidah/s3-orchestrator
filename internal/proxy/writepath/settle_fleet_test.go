// -------------------------------------------------------------------------------
// Cleanup Settle Fleet Tests
//
// Author: Alex Freidah
//
// Settles queued cleanup rows through a coordinator over a live backend
// runtime, so the row outcomes are asserted against real backend deletes:
// retired once the bytes are gone, retried with backoff when the delete
// fails, and retired without a delete when the backend is not registered.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/cleanup"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// fleetBackend is the one backend the settle fleet holds.
const fleetBackend = "b1"

// TestSettle_FleetRetiresDeletedRows verifies a row whose bytes the backend
// deleted is retired and the bytes are gone.
func TestSettle_FleetRetiresDeletedRows(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().CompleteCleanupItem(gomock.Any(), int64(1)).Return(nil).Times(1)
	storetest.Permissive(store)

	be := backendtest.NewInMemory()
	be.Objects["k!orphan"] = backendtest.Object{}
	coord, _ := newFleet(t, store, map[string]backend.ObjectBackend{fleetBackend: be}, nil)
	coord.Settle(context.Background(), []core.CleanupItem{
		cleanupRow(1, fleetBackend, "k!orphan", core.CleanupReasonDeleted, 4),
	})

	if be.Has("k!orphan") {
		t.Error("a settled row's bytes are still on the backend")
	}
}

// TestSettle_FleetRetriesAFailedDelete verifies a delete the backend failed
// leaves its row queued for a retry after the backoff its attempts call for.
func TestSettle_FleetRetriesAFailedDelete(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().RetryCleanupItem(gomock.Any(), int64(1), cleanup.Backoff(2), gomock.Any()).Return(nil).Times(1)
	store.EXPECT().CompleteCleanupItem(gomock.Any(), gomock.Any()).Times(0)
	storetest.Permissive(store)

	be := backendtest.NewInMemory()
	be.Objects["k!orphan"] = backendtest.Object{}
	be.DeleteErr = errors.New("connection refused")
	coord, _ := newFleet(t, store, map[string]backend.ObjectBackend{fleetBackend: be}, nil)
	row := cleanupRow(1, fleetBackend, "k!orphan", core.CleanupReasonDeleted, 4)
	row.Attempts = 2
	coord.Settle(context.Background(), []core.CleanupItem{row})

	if !be.Has("k!orphan") {
		t.Error("a failed delete removed the bytes anyway")
	}
}

// TestSettle_FleetRetiresRowsOnAnUnknownBackend verifies a row naming a backend
// the fleet no longer has is retired without any delete.
func TestSettle_FleetRetiresRowsOnAnUnknownBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().CompleteCleanupItem(gomock.Any(), int64(8)).Return(nil).Times(1)
	storetest.Permissive(store)

	coord, _ := newFleet(t, store, map[string]backend.ObjectBackend{fleetBackend: backendtest.NewInMemory()}, nil)
	coord.Settle(context.Background(), []core.CleanupItem{
		cleanupRow(8, "retired-backend", "k!orphan", core.CleanupReasonDeleted, 4),
	})
}
