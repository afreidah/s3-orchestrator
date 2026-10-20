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

// expectCopyClaims has the store claim the first n ranked candidates and
// decline the rest, and records the ranking and flags it was handed.
func expectCopyClaims(store *MockCoordinatorStores, n int, ranked *[]string, accepts *core.BackendFlags) {
	store.EXPECT().ClaimPendingCopies(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, intents []*core.PendingObject, names []string, flags core.BackendFlags) (core.CopyClaims, error) {
			*ranked, *accepts = names, flags
			var out core.CopyClaims
			for i, name := range names {
				if i < n && i < len(intents) {
					intents[i].BackendName = name
					out.Claimed = append(out.Claimed, intents[i])
				} else {
					out.Declined = append(out.Declined, name)
				}
			}
			return out, nil
		})
}

// TestClaimWriteCopies_ReturnsWhatTheStoreClaimed asserts the copies are
// claimed in one store call over the ranked candidates with the write's
// flags, and the intents come back on the backends they were claimed on.
func TestClaimWriteCopies_ReturnsWhatTheStoreClaimed(t *testing.T) {
	t.Parallel()
	store := NewMockCoordinatorStores(gomock.NewController(t))
	var (
		ranked  []string
		accepts core.BackendFlags
	)
	expectCopyClaims(store, 2, &ranked, &accepts)
	coord := newCoordinatorWithStore(store)

	got, err := coord.ClaimWriteCopies(context.Background(), twoIntents("k", 4096), []string{"b1", "b2", "b3"})
	if err != nil {
		t.Fatalf("ClaimWriteCopies: %v", err)
	}
	if want := []string{"b1", "b2", "b3"}; !slices.Equal(ranked, want) || accepts != core.AcceptForWrite {
		t.Errorf("store got %v with %v, want %v with %v", ranked, accepts, want, core.AcceptForWrite)
	}
	if len(got) != 2 || got[0].BackendName != "b1" || got[1].BackendName != "b2" {
		t.Errorf("claimed %+v, want the copies on b1 and b2", got)
	}
}

// TestClaimWriteCopies_FewerBackendsThanCopies asserts a fleet that cannot take
// every copy still takes the ones it can. The shortfall is the replicator's,
// and failing the write over it would be worse than one copy today.
func TestClaimWriteCopies_FewerBackendsThanCopies(t *testing.T) {
	t.Parallel()
	store := NewMockCoordinatorStores(gomock.NewController(t))
	var (
		ranked  []string
		accepts core.BackendFlags
	)
	expectCopyClaims(store, 1, &ranked, &accepts)
	coord := newCoordinatorWithStore(store)

	got, err := coord.ClaimWriteCopies(context.Background(), twoIntents("k", 4096), []string{"b1", "b2"})
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
	store := NewMockCoordinatorStores(gomock.NewController(t))
	var (
		ranked  []string
		accepts core.BackendFlags
	)
	expectCopyClaims(store, 0, &ranked, &accepts)
	coord := newCoordinatorWithStore(store)

	if _, err := coord.ClaimWriteCopies(context.Background(), twoIntents("k", 4096),
		[]string{"b1", "b2"}); !errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Errorf("err = %v, want ErrNoSpaceAvailable", err)
	}
}

// TestClaimWriteCopies_ClaimErrorFailsTheWrite asserts a database error
// surfaces, since the transaction claimed nothing and reading an outage as
// "no room" would place the write somewhere else.
func TestClaimWriteCopies_ClaimErrorFailsTheWrite(t *testing.T) {
	t.Parallel()
	store := NewMockCoordinatorStores(gomock.NewController(t))
	store.EXPECT().ClaimPendingCopies(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(core.CopyClaims{}, errors.New("db down"))
	coord := newCoordinatorWithStore(store)

	got, err := coord.ClaimWriteCopies(context.Background(), twoIntents("k", 4096), []string{"b1", "b2"})
	if err == nil {
		t.Fatal("expected the claim failure to surface")
	}
	if errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Error("a database error was reported as a full fleet")
	}
	if got != nil {
		t.Errorf("claimed %d copies alongside the error, want none", len(got))
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
		Quota:    counter.NewQuotaTracker(),
	}), store)

	recorded, err := coord.CommitCompanionCopy(context.Background(), companionIntent("b2"))
	if err != nil {
		t.Fatalf("CommitCompanionCopy: %v", err)
	}
	if !recorded {
		t.Error("a committed copy was not reported as recorded")
	}
	if got := loadUsage(usage.Backend(), "b2").IngressBytes; got != 4096 {
		t.Errorf("b2 ingress = %d, want the 4096 the copy landed", got)
	}
}

// TestCommitCompanionCopy_DiscardsAnUntrustedCopy asserts a copy the store
// could not vouch for has its own bytes removed from the backend, and only
// those. A newer write took the key while this upload ran, so these bytes are
// stale. The copy the newer write committed sits at a different path on the
// same backend and must survive untouched.
func TestCommitCompanionCopy_DiscardsAnUntrustedCopy(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().CommitCompanionCopy(gomock.Any(), gomock.Any()).
		Return(core.CompanionCopyUntrusted,
			[]core.CleanupItem{cleanupRow(7, "b2", "k!loser", core.CleanupReasonCompanionUntrusted, 4096)},
			nil, nil)
	store.EXPECT().CompleteCleanupItems(gomock.Any(), retiring(7)).DoAndReturn(retireAll)

	be := backendtest.NewInMemory()
	for _, path := range []string{"k!loser", "k!winner"} {
		if _, err := be.PutObject(context.Background(), path, strings.NewReader("bytes"), 5, "text/plain", nil); err != nil {
			t.Fatalf("seed backend at %s: %v", path, err)
		}
	}
	coord := newCoordinatorWithBackend("b2", be, store)

	intent := companionIntent("b2")
	intent.StorageKey = "k!loser"
	recorded, err := coord.CommitCompanionCopy(context.Background(), intent)
	if err != nil {
		t.Fatalf("CommitCompanionCopy: %v", err)
	}
	if recorded {
		t.Error("a discarded copy was reported as recorded, which would count it toward the factor")
	}
	if be.HasCopyOf("k!loser") {
		t.Error("the untrusted copy's bytes are still on the backend")
	}
	if !be.HasCopyOf("k!winner") {
		t.Error("the discard deleted the committed copy's bytes")
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
	if !be.HasCopyOf("k") {
		t.Error("a database error took the copy's bytes with it")
	}
}
