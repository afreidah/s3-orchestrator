// -------------------------------------------------------------------------------
// Overwrite Race - Two Writes Racing to One Backend
//
// Author: Alex Freidah
//
// Runs two overwrites of one key racing towards one backend end to end, with
// the losing write's companion resolving after the winner's has committed. It
// uses a real store and a real backend rather than mocks, because the failure
// it guards against is a disagreement between the ledger and the bytes, and
// only a test that holds both can see it.
//
// The race is scheduled in program order rather than with goroutines. Only one
// interleaving matters (the loser resolves last), so running it deliberately
// makes the test give the same answer on every run.
// -------------------------------------------------------------------------------

package writepath

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/sqlite"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// -------------------------------------------------------------------------
// FIXTURE
// -------------------------------------------------------------------------

// raceKey is the key both writes contend for, namespaced like any stored
// object so the minted storage keys look like real ones.
const raceKey = "bucket/repro/contended"

// The two backends the write places on: the one that answers the client, and
// the slow one both companions are still uploading to when the second write
// arrives. Only the second one matters to the assertions.
const (
	fastBackend = "fast"
	slowBackend = "slow"
)

// raceFleet builds a coordinator over a real SQLite store and real in-memory
// backends. Nothing is stubbed, because the discard under test is a store
// transaction and a fake store would only restate the code being checked.
func raceFleet(t *testing.T) (*Coordinator, *sqlite.Store, *backendtest.InMemory) {
	t.Helper()
	ctx := context.Background()

	store, err := sqlite.NewStore(ctx, &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"}, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: fastBackend, QuotaBytes: 1 << 30},
		{Name: slowBackend, QuotaBytes: 1 << 30},
	}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}

	names := []string{fastBackend, slowBackend}
	slow := backendtest.NewInMemory()
	rt := infra.New(&infra.Config{
		Backends:        map[string]backend.ObjectBackend{fastBackend: backendtest.NewInMemory(), slowBackend: slow},
		Order:           names,
		BackendTimeout:  fleetTimeout,
		Usage:           counter.NewUsageTracker(counter.NewLocalCounterBackend(names), nil),
		Quota:           counter.NewQuotaTracker(names),
		RoutingStrategy: config.RoutingPack,
	})
	return New(rt, store), store, slow
}

// write is one client PUT of the contended key, in the shape parallel_copies
// gives it: a primary copy on the fast backend that answers the client, and a
// companion still uploading to the slow one.
type write struct {
	primary   *core.PendingObject
	companion *core.PendingObject
	body      []byte
}

// startWrite claims both intents without uploading anything. The companion's
// bytes land when the test calls landCompanion, because their timing relative
// to the other write is what the tests are about.
func startWrite(t *testing.T, coord *Coordinator, body string) *write {
	t.Helper()
	ctx := context.Background()
	payload := []byte(body)

	w := &write{body: payload}
	w.primary = NewPendingIntent(raceKey, int64(len(payload)), nil, nil)
	if _, err := coord.ClaimWriteTarget(ctx, w.primary, []string{fastBackend}); err != nil {
		t.Fatalf("claim the primary copy: %v", err)
	}
	w.companion = NewPendingIntent(raceKey, int64(len(payload)), nil, nil)
	w.companion.Role = core.PendingRoleCompanion
	if _, err := coord.ClaimWriteCopies(ctx, []*core.PendingObject{w.companion}, []string{slowBackend}); err != nil {
		t.Fatalf("claim the companion copy: %v", err)
	}
	return w
}

// landCompanion completes the companion upload the client never waits for, at
// the companion intent's own path.
func landCompanion(t *testing.T, slow *backendtest.InMemory, w *write) {
	t.Helper()
	if _, err := slow.PutObject(context.Background(), w.companion.StorageKey,
		bytes.NewReader(w.body), int64(len(w.body)), "application/octet-stream", nil); err != nil {
		t.Fatalf("upload the companion copy: %v", err)
	}
}

// commitPrimary records the copy that answers the client, declaring the
// companion as still in flight so the commit keeps its intent.
func commitPrimary(t *testing.T, coord *Coordinator, w *write) {
	t.Helper()
	if err := coord.RecordObjectAndPromoteIntent(context.Background(), noopSpan(), &core.RecordObjectRequest{
		Key:  raceKey,
		Size: int64(len(w.body)),
		Copies: []core.ObjectCopy{{
			Backend: fastBackend, IntentID: w.primary.IntentID, StorageKey: w.primary.StorageKey,
		}},
		Placing: []core.ObjectCopy{{
			Backend: slowBackend, IntentID: w.companion.IntentID, StorageKey: w.companion.StorageKey,
		}},
	}); err != nil {
		t.Fatalf("commit the primary copy: %v", err)
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestOverwriteRace_LoserDiscardLeavesTheWinnerIntact runs two overwrites of
// one key that each leave a companion uploading to the slow backend. The
// second write commits, its companion commits, and only then does the first
// write's companion resolve and discard itself.
//
// The discard must remove only the loser's bytes. The test checks that the
// winner's row and the bytes it describes both survive and still agree.
func TestOverwriteRace_LoserDiscardLeavesTheWinnerIntact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	coord, store, slow := raceFleet(t)

	// Both writes are answered on the fast backend while their companions are
	// still uploading to the slow one.
	loser := startWrite(t, coord, "v2-the-overtaken-write")
	commitPrimary(t, coord, loser)

	winner := startWrite(t, coord, "v3-the-write-that-took-the-key")
	// This commit clears the loser's companion intent and hands its bytes to
	// cleanup, which finds nothing because that upload has not landed yet. That
	// leaves the discard below as the only thing that can remove them.
	commitPrimary(t, coord, winner)

	// The winner's companion lands and commits. From here the ledger names a
	// copy on the slow backend, and it is the one that must survive.
	landCompanion(t, slow, winner)
	recorded, err := coord.CommitCompanionCopy(ctx, winner.companion)
	if err != nil {
		t.Fatalf("commit the winning companion: %v", err)
	}
	if !recorded {
		t.Fatal("the winning companion was not recorded, so the race under test never happened")
	}

	// Only now does the overtaken upload finish and resolve. The winner's
	// commit cleared its intent, so it discards itself.
	landCompanion(t, slow, loser)
	if recorded, err = coord.CommitCompanionCopy(ctx, loser.companion); err != nil {
		t.Fatalf("resolve the overtaken companion: %v", err)
	}
	if recorded {
		t.Fatal("an overtaken companion was recorded, which would count a stale copy toward the factor")
	}

	// The row: still there, still naming the winner's path.
	locs, err := store.GetAllObjectLocations(ctx, raceKey)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	row, ok := copyOn(locs, slowBackend)
	if !ok {
		t.Fatalf("the ledger lost the copy on %s entirely: %+v", slowBackend, locs)
	}
	if row.StorageKey != winner.companion.StorageKey {
		t.Errorf("row names %q, want the winning companion's %q", row.StorageKey, winner.companion.StorageKey)
	}

	// The bytes: still there, and still the winner's.
	stored, ok := slow.Get(winner.companion.StorageKey)
	if !ok {
		t.Fatal("the ledger names a copy on the slow backend that does not exist")
	}
	if !bytes.Equal(stored.Data, winner.body) {
		t.Errorf("the surviving bytes are %q, want the winner's %q", stored.Data, winner.body)
	}

	// The loser's own bytes are the ones removed.
	if slow.Has(loser.companion.StorageKey) {
		t.Error("the overtaken companion's bytes were left on the backend")
	}

	// Nothing of either write is still holding the backend: both primaries
	// committed, both companions resolved.
	if depth, derr := store.PendingDepth(ctx); derr != nil || depth != 0 {
		t.Errorf("pending intents = %d (err %v), want none left", depth, derr)
	}
}

// TestOverwriteRace_OverwriteDoesNotMutateInPlace verifies two writes of one
// key never share a path on a backend. Every cleanup deletes one of those
// paths, so a shared path is the only way a cleanup could reach another
// write's bytes.
func TestOverwriteRace_OverwriteDoesNotMutateInPlace(t *testing.T) {
	t.Parallel()
	coord, _, slow := raceFleet(t)

	first := startWrite(t, coord, "first")
	second := startWrite(t, coord, "second")
	landCompanion(t, slow, first)
	landCompanion(t, slow, second)

	if first.companion.StorageKey == second.companion.StorageKey {
		t.Fatalf("two writes of %q share the path %q", raceKey, first.companion.StorageKey)
	}
	for _, path := range []string{first.companion.StorageKey, second.companion.StorageKey} {
		if path == raceKey {
			t.Errorf("a write stored its bytes at the object's own key (%q)", path)
		}
		if !strings.HasPrefix(path, raceKey) {
			t.Errorf("path %q does not sit under the object's key, so a bucket listing scatters it", path)
		}
	}
	// Both are still there: the second write did not overwrite the first's
	// bytes, it put its own somewhere else.
	if !slow.Has(first.companion.StorageKey) || !slow.Has(second.companion.StorageKey) {
		t.Error("one write's bytes replaced the other's in place")
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// noopSpan is the span the commit helpers report on. These tests assert on the
// store and the backend, so the span only has to exist.
func noopSpan() trace.Span {
	_, span := noop.NewTracerProvider().Tracer("overwrite-race").Start(context.Background(), "commit")
	return span
}

// copyOn finds the copy recorded on one backend.
func copyOn(locs []core.ObjectLocation, backendName string) (core.ObjectLocation, bool) {
	for i := range locs {
		if locs[i].BackendName == backendName {
			return locs[i], true
		}
	}
	return core.ObjectLocation{}, false
}
