// -------------------------------------------------------------------------------
// Write Coordinator Tests - Copies Placed By The Write
//
// Author: Alex Freidah
//
// Covers the two helpers a write placing its own copies runs through: claiming
// a backend per copy, and committing a copy whose upload outlived the response.
// Both decide what happens to bytes already on a backend, so the assertions are
// about which backend was claimed and what was cleaned up rather than about
// return values alone.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// twoIntents builds the intents one write of size bytes would claim for its
// copies, which the coordinator fills the backend in.
func twoIntents(key string, size int64) []*core.PendingObject {
	primary := NewPendingIntent(key, size, nil, nil)
	companion := NewPendingIntent(key, size, nil, nil)
	companion.Role = core.PendingRoleCompanion
	return []*core.PendingObject{primary, companion}
}

// -------------------------------------------------------------------------
// CLAIMING A BACKEND PER COPY
// -------------------------------------------------------------------------

// TestClaimWriteCopies_ClaimsOnePerIntent asserts each copy is claimed on a
// backend of its own, in the order the ranking proposed.
func TestClaimWriteCopies_ClaimsOnePerIntent(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)

	var claimed []string
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.PendingObject) (bool, error) {
			claimed = append(claimed, p.BackendName)
			return true, nil
		}).Times(2)

	coord := newCoordinatorWithStore(store)

	got, err := coord.ClaimWriteCopies(context.Background(), twoIntents("k", 4096), []string{"b1", "b2", "b3"})
	if err != nil {
		t.Fatalf("ClaimWriteCopies: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("claimed %d copies, want 2", len(got))
	}
	if want := []string{"b1", "b2"}; !slices.Equal(claimed, want) {
		t.Errorf("claimed %v, want %v: the third candidate was not needed", claimed, want)
	}
	if got[0].BackendName != "b1" || got[1].BackendName != "b2" {
		t.Errorf("intents carry %s and %s, want the backends they were claimed on", got[0].BackendName, got[1].BackendName)
	}
}

// TestClaimWriteCopies_SkipsABackendWithoutRoom asserts a declining backend is
// passed over for the next candidate rather than costing the write a copy.
func TestClaimWriteCopies_SkipsABackendWithoutRoom(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)

	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.PendingObject) (bool, error) {
			return p.BackendName != "b2", nil
		}).Times(3)

	coord := newCoordinatorWithStore(store)

	got, err := coord.ClaimWriteCopies(context.Background(), twoIntents("k", 4096), []string{"b1", "b2", "b3"})
	if err != nil {
		t.Fatalf("ClaimWriteCopies: %v", err)
	}
	if len(got) != 2 || got[1].BackendName != "b3" {
		t.Fatalf("claimed %d copies ending on %q, want 2 ending on b3", len(got), got[len(got)-1].BackendName)
	}
}

// TestClaimWriteCopies_FewerBackendsThanCopies asserts a fleet that cannot take
// every copy still takes the ones it can. The shortfall is the replicator's,
// and failing the write over it would be worse than one copy today.
func TestClaimWriteCopies_FewerBackendsThanCopies(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).Return(true, nil).Times(1)

	coord := newCoordinatorWithStore(store)

	got, err := coord.ClaimWriteCopies(context.Background(), twoIntents("k", 4096), []string{"b1"})
	if err != nil {
		t.Fatalf("ClaimWriteCopies: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("claimed %d copies, want the 1 the fleet could take", len(got))
	}
}

// TestClaimWriteCopies_NoCandidateFits asserts a write with nowhere to go is
// told so, rather than being handed an empty set to upload to.
func TestClaimWriteCopies_NoCandidateFits(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).Return(false, nil).Times(2)

	coord := newCoordinatorWithStore(store)

	if _, err := coord.ClaimWriteCopies(context.Background(), twoIntents("k", 4096),
		[]string{"b1", "b2"}); !errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Errorf("err = %v, want ErrNoSpaceAvailable", err)
	}
}

// TestClaimWriteCopies_FirstClaimErrorFailsTheWrite asserts a database error
// before anything is claimed surfaces, since the write has no copy to fall back
// on and reading an outage as "no room" would place it somewhere else.
func TestClaimWriteCopies_FirstClaimErrorFailsTheWrite(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).
		Return(false, errors.New("db down")).Times(1)

	coord := newCoordinatorWithStore(store)

	got, err := coord.ClaimWriteCopies(context.Background(), twoIntents("k", 4096), []string{"b1", "b2"})
	if err == nil {
		t.Fatal("expected the insert failure to surface")
	}
	if errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Error("a database error was reported as a full fleet")
	}
	if got != nil {
		t.Errorf("claimed %d copies alongside the error, want none", len(got))
	}
}

// TestClaimWriteCopies_LaterClaimErrorKeepsWhatItHas asserts a database error
// after a copy is claimed ends the claiming rather than the write: one copy is
// what a write places today, and the replicator makes up the difference.
func TestClaimWriteCopies_LaterClaimErrorKeepsWhatItHas(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)

	gomock.InOrder(
		store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).Return(true, nil),
		store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).Return(false, errors.New("db down")),
	)

	coord := newCoordinatorWithStore(store)

	got, err := coord.ClaimWriteCopies(context.Background(), twoIntents("k", 4096), []string{"b1", "b2", "b3"})
	if err != nil {
		t.Fatalf("ClaimWriteCopies: %v", err)
	}
	if len(got) != 1 || got[0].BackendName != "b1" {
		t.Errorf("kept %d copies, want the one claimed before the error", len(got))
	}
}

// -------------------------------------------------------------------------
// COMMITTING A COPY THAT OUTLIVED THE RESPONSE
// -------------------------------------------------------------------------

// companionIntent is the intent one of a write's further copies commits under.
func companionIntent(backendName string) *core.PendingObject {
	p := NewPendingIntent("k", 4096, nil, nil)
	p.BackendName = backendName
	p.Role = core.PendingRoleCompanion
	return p
}

// TestCommitCompanionCopy_RecordsTheCopy asserts a copy whose intent is still
// there is recorded and its bytes charged to the backend, with nothing deleted.
func TestCommitCompanionCopy_RecordsTheCopy(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().CommitCompanionCopy(gomock.Any(), gomock.Any()).
		Return(core.CompanionCopyCommitted, nil, core.QuotaDeltas{"b2": 4096}, nil)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b2"}), nil)
	coord := New(infra.New(&infra.Config{
		Backends: map[string]s3be.ObjectBackend{"b2": backendtest.NewInMemory()},
		Usage:    usage,
		Quota:    counter.NewQuotaTracker([]string{"b2"}),
	}), store)

	recorded, err := coord.CommitCompanionCopy(context.Background(), companionIntent("b2"))
	if err != nil {
		t.Fatalf("CommitCompanionCopy: %v", err)
	}
	if !recorded {
		t.Error("a committed copy was not reported as recorded")
	}
	if got := usage.Backend().Load("b2", counter.FieldIngressBytes); got != 4096 {
		t.Errorf("b2 ingress = %d, want the 4096 the copy landed", got)
	}
}

// TestCommitCompanionCopy_DiscardsAnUntrustedCopy asserts a copy the store
// could not vouch for has its bytes removed from the backend. A newer write
// took the key while this upload ran, so what sits at that path is either
// version and a read of it would be silently wrong.
func TestCommitCompanionCopy_DiscardsAnUntrustedCopy(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().CommitCompanionCopy(gomock.Any(), gomock.Any()).
		Return(core.CompanionCopyUntrusted,
			[]core.DeletedCopy{{BackendName: "b2", SizeBytes: 4096, Reason: core.CleanupReasonCompanionUntrusted}},
			nil, nil)

	be := backendtest.NewInMemory()
	if _, err := be.PutObject(context.Background(), "k", strings.NewReader("bytes"), 5, "text/plain", nil); err != nil {
		t.Fatalf("seed backend: %v", err)
	}
	coord := newCoordinatorWithBackend("b2", be, store)

	recorded, err := coord.CommitCompanionCopy(context.Background(), companionIntent("b2"))
	if err != nil {
		t.Fatalf("CommitCompanionCopy: %v", err)
	}
	if recorded {
		t.Error("a discarded copy was reported as recorded, which would count it toward the factor")
	}
	if be.Has("k") {
		t.Error("the untrusted copy's bytes are still on the backend")
	}
}

// TestCommitCompanionCopy_StoreErrorLeavesTheIntent asserts a failed commit
// surfaces and deletes nothing: the intent stays, and the reaper resolves the
// copy on a later tick.
func TestCommitCompanionCopy_StoreErrorLeavesTheIntent(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().CommitCompanionCopy(gomock.Any(), gomock.Any()).
		Return(core.CompanionCopyCommitted, nil, nil, errors.New("db down"))

	be := backendtest.NewInMemory()
	if _, err := be.PutObject(context.Background(), "k", strings.NewReader("bytes"), 5, "text/plain", nil); err != nil {
		t.Fatalf("seed backend: %v", err)
	}
	coord := newCoordinatorWithBackend("b2", be, store)

	if _, err := coord.CommitCompanionCopy(context.Background(), companionIntent("b2")); err == nil {
		t.Fatal("expected the commit failure to surface")
	}
	if !be.Has("k") {
		t.Error("a database error took the copy's bytes with it")
	}
}
