// -------------------------------------------------------------------------------
// PutObject Fan-Out Tests
//
// Author: Alex Freidah
//
// Covers a write that places its own copies: every backend claimed gets the
// bytes, the client is answered on the first copy rather than the last, and a
// shortfall is left to the replicator instead of failing the write.
//
// The copies after the first commit on goroutines that outlive the response, so
// the assertions about them poll rather than read once.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
)

// settleWindow bounds how long a test waits for the copies that outlive the
// response. Generous because it covers a goroutine handoff on a loaded CI box,
// and the assertions poll, so a fast machine does not pay it.
const settleWindow = 2 * time.Second

// TestPutObject_ParallelCopies_PlacesEveryCopy asserts a write configured for
// three copies puts the bytes on three backends itself, records one of them as
// the object, and commits the other two as further copies of it.
func TestPutObject_ParallelCopies_PlacesEveryCopy(t *testing.T) {
	t.Parallel()
	b1, b2, b3 := backendtest.NewInMemory(), backendtest.NewInMemory(), backendtest.NewInMemory()

	store, c := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2, "b3": b3}, &fleetOpts{
		Order:          []string{"b1", "b2", "b3"},
		CopiesPerWrite: 3,
	})

	payload := []byte("a copy on every backend the write claimed")
	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: "key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	for name, be := range map[string]*backendtest.InMemory{"b1": b1, "b2": b2, "b3": b3} {
		testx.Eventually(t, settleWindow, func() bool { return be.Has("key") },
			"backend %s never received its copy", name)
	}

	testx.Eventually(t, settleWindow, func() bool {
		_, companions, _ := c.snapshot()
		return len(companions) == 2
	}, "the two copies after the first never committed")

	recorded, companions, _ := c.snapshot()
	if len(recorded) != 1 {
		t.Fatalf("recorded %d objects, want 1: the first copy records, the rest add themselves", len(recorded))
	}
	// Every copy's bytes go on every backend, so each one reads back whole.
	for name, be := range map[string]*backendtest.InMemory{"b1": b1, "b2": b2, "b3": b3} {
		obj, _ := be.Get("key")
		if !bytes.Equal(obj.Data, payload) {
			t.Errorf("backend %s holds %d bytes, want %d", name, len(obj.Data), len(payload))
		}
	}
	// One backend per copy. Which of them recorded the object is whichever
	// upload landed first, so the assertion is that no backend took two.
	seen := map[string]bool{recorded[0].Backend: true}
	for _, p := range companions {
		if seen[p.BackendName] {
			t.Errorf("backend %s took two of the write's copies", p.BackendName)
		}
		seen[p.BackendName] = true
	}
}

// TestCopyIntents_OnePrimaryTheRestCompanions asserts the roles a write claims
// its copies under. The role decides what a reaper does with an intent left by
// a process that died: it promotes the primary, so the object survives, and
// discards the companions, whose bytes it cannot tell apart from an older
// object at the same path. Which intent commits the object at runtime is a
// different question - that is whichever upload lands first.
func TestCopyIntents_OnePrimaryTheRestCompanions(t *testing.T) {
	t.Parallel()
	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{
		CopiesPerWrite: 3,
	})

	plan := &putPlan{uploadSize: 100, etagDigest: "d"}
	intents := mgr.copyIntents(&PutObjectRequest{Key: "k"}, plan)

	if len(intents) != 3 {
		t.Fatalf("built %d intents, want one per copy", len(intents))
	}
	if got := intents[0].RoleOrDefault(); got != core.PendingRolePrimary {
		t.Errorf("first intent role = %q, want primary", got)
	}
	for _, p := range intents[1:] {
		if p.Role != core.PendingRoleCompanion {
			t.Errorf("further intent role = %q, want companion", p.Role)
		}
	}
	seen := map[string]bool{}
	for _, p := range intents {
		if seen[p.IntentID] {
			t.Fatalf("intent id %s was reused across copies", p.IntentID)
		}
		seen[p.IntentID] = true
	}
}

// TestPutObject_ParallelCopies_KeepsTheIntentsStillUploading asserts the commit
// of the first copy carries the copies still uploading, which is what keeps
// their intents from being cleared with every other intent for the key and
// their backends out of the displacement the commit performs.
func TestPutObject_ParallelCopies_KeepsTheIntentsStillUploading(t *testing.T) {
	t.Parallel()
	fast := backendtest.NewInMemory()
	slow := &slowMockBackend{InMemory: backendtest.NewInMemory(), delay: 150 * time.Millisecond}

	store, c := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"fast": fast, "slow": slow}, &fleetOpts{
		Order:          []string{"fast", "slow"},
		CopiesPerWrite: 2,
		BackendTimeout: 5 * time.Second,
	})

	payload := []byte("the slow copy's intent has to survive the fast one's commit")
	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: "key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	recorded, _, _ := c.snapshot()
	if len(recorded) != 1 {
		t.Fatalf("recorded %d objects, want 1", len(recorded))
	}
	if len(recorded[0].Placing) != 1 {
		t.Fatalf("commit carried %d copies still uploading, want the 1 in flight", len(recorded[0].Placing))
	}
	if got := recorded[0].Placing[0].Backend; got != "slow" {
		t.Errorf("commit named %q as still uploading, want the slow backend", got)
	}

	testx.Eventually(t, settleWindow, func() bool {
		_, companions, _ := c.snapshot()
		return len(companions) == 1
	}, "the slow copy never committed")

	_, companions, _ := c.snapshot()
	if got, want := companions[0].IntentID, recorded[0].Placing[0].IntentID; got != want {
		t.Errorf("the copy that committed carried intent %s, but the commit kept %s", got, want)
	}
}

// TestPutObject_ParallelCopies_AnswersOnTheFirstCopy asserts the client is not
// made to wait for the slowest backend. Waiting for every copy would put that
// backend on the critical path of every write, which is what placing one copy
// and repairing later exists to avoid.
func TestPutObject_ParallelCopies_AnswersOnTheFirstCopy(t *testing.T) {
	t.Parallel()
	const slowUpload = 750 * time.Millisecond
	fast := backendtest.NewInMemory()
	slow := &slowMockBackend{InMemory: backendtest.NewInMemory(), delay: slowUpload}

	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"fast": fast, "slow": slow}, &fleetOpts{
		Order:          []string{"fast", "slow"},
		CopiesPerWrite: 2,
		BackendTimeout: 5 * time.Second,
	})

	payload := []byte("answered on the first copy")
	start := time.Now()
	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: "key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= slowUpload {
		t.Errorf("PutObject took %s, which is the slow backend's %s: it waited for every copy", elapsed, slowUpload)
	}
	if !fast.Has("key") {
		t.Error("the copy that answered the client is not on its backend")
	}

	// The slow copy still lands: the response detached it, it did not cancel it.
	testx.Eventually(t, settleWindow, func() bool { return slow.Has("key") },
		"the copy still uploading at response time never finished")
}

// TestPutObject_ParallelCopies_ShortfallIsNotAFailure asserts a write asked for
// more copies than the fleet will take places what it can. The replicator
// fills the rest, which is what it does for every copy today.
func TestPutObject_ParallelCopies_ShortfallIsNotAFailure(t *testing.T) {
	t.Parallel()
	b1, b2 := backendtest.NewInMemory(), backendtest.NewInMemory()

	store, c := eligibleStore(t)
	c.full = map[string]bool{"b2": true}
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{
		Order:          []string{"b1", "b2"},
		CopiesPerWrite: 2,
	})

	payload := []byte("one backend has room, the other does not")
	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: "key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if !b1.Has("key") {
		t.Error("the copy that could be placed is missing")
	}
	if b2.Has("key") {
		t.Error("a backend that declined the claim was written to anyway")
	}
	recorded, _, _ := c.snapshot()
	if len(recorded) != 1 {
		t.Errorf("recorded %d objects, want 1", len(recorded))
	}
}

// TestPutObject_ParallelCopies_FailsWhenNoCopyLands asserts the write fails
// only when every copy does, which is what a single-copy write does today.
func TestPutObject_ParallelCopies_FailsWhenNoCopyLands(t *testing.T) {
	t.Parallel()
	b1, b2 := backendtest.NewInMemory(), backendtest.NewInMemory()
	b1.PutErr = errors.New("b1 down")
	b2.PutErr = errors.New("b2 down")

	store, c := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{
		Order:          []string{"b1", "b2"},
		CopiesPerWrite: 2,
	})

	payload := []byte("nowhere to land")
	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: "key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain",
	}); err == nil {
		t.Fatal("expected the write to fail once every copy failed")
	}
	recorded, companions, _ := c.snapshot()
	if len(recorded) != 0 || len(companions) != 0 {
		t.Errorf("recorded %d objects and %d further copies for a write that landed nothing", len(recorded), len(companions))
	}
}

// TestPutObject_ParallelCopies_CommitFailureAbandonsTheRest asserts a failed
// commit fails the write and leaves the copies still uploading to the reaper
// rather than letting them record themselves. Nothing anchors the key once the
// first commit is gone, so a copy that recorded itself would be a copy of an
// object no row describes.
func TestPutObject_ParallelCopies_CommitFailureAbandonsTheRest(t *testing.T) {
	t.Parallel()
	fast := backendtest.NewInMemory()
	slow := &slowMockBackend{InMemory: backendtest.NewInMemory(), delay: 100 * time.Millisecond}

	c := &objectsCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjRecord(c, errors.New("db write failed"))).AnyTimes()
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjInsertPending(c, nil)).AnyTimes()
	store.EXPECT().CommitCompanionCopy(gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjCommitCompanion(c)).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"fast": fast, "slow": slow}, &fleetOpts{
		Order:          []string{"fast", "slow"},
		CopiesPerWrite: 2,
		BackendTimeout: 5 * time.Second,
	})

	payload := []byte("the commit fails, so nothing anchors the key")
	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: "key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain",
	}); err == nil {
		t.Fatal("expected the write to fail when its commit did")
	}

	// The slow copy lands on its backend and is then abandoned: its intent is
	// left for the reaper, which discards an extra copy rather than promoting.
	testx.Eventually(t, settleWindow, func() bool { return slow.Has("key") },
		"the copy still uploading never finished")
	testx.Eventually(t, settleWindow, func() bool {
		_, companions, _ := c.snapshot()
		return len(companions) == 0
	}, "an abandoned copy recorded itself against a key nothing anchors")
}

// TestPutObject_ParallelCopies_DrainRaceDropsThatCopy asserts a backend that
// started draining while a copy was uploading does not get recorded. The drain
// deletes what it holds, so a copy committed after it started would be a row
// pointing at bytes on their way out.
func TestPutObject_ParallelCopies_DrainRaceDropsThatCopy(t *testing.T) {
	t.Parallel()
	b1, b2 := backendtest.NewInMemory(), backendtest.NewInMemory()

	store, c := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{
		Order:          []string{"b1", "b2"},
		CopiesPerWrite: 2,
		Draining:       []string{"b2"},
	})

	payload := []byte("one backend starts draining mid-write")
	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: "key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	recorded, _, _ := c.snapshot()
	if len(recorded) != 1 || recorded[0].Backend != "b1" {
		t.Fatalf("recorded %+v, want the single copy on b1", recorded)
	}
	testx.Eventually(t, settleWindow, func() bool {
		_, companions, _ := c.snapshot()
		return len(companions) == 0
	}, "the copy on the draining backend was recorded anyway")
}

// TestPutObject_ParallelCopies_SurvivesAnUploadThatOutlivesTheRequest asserts a
// cancelled request does not take the copies still uploading with it. The
// client has already been answered, so the copy is owed to the object rather
// than to the request.
func TestPutObject_ParallelCopies_SurvivesAnUploadThatOutlivesTheRequest(t *testing.T) {
	t.Parallel()
	fast := backendtest.NewInMemory()
	slow := &slowMockBackend{InMemory: backendtest.NewInMemory(), delay: 200 * time.Millisecond}

	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"fast": fast, "slow": slow}, &fleetOpts{
		Order:          []string{"fast", "slow"},
		CopiesPerWrite: 2,
		BackendTimeout: 5 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	payload := []byte("the request goes away, the copy does not")
	if _, err := mgr.PutObject(ctx, &PutObjectRequest{
		Key: "key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	cancel()

	testx.Eventually(t, settleWindow, func() bool { return slow.Has("key") },
		"cancelling the request killed a copy the client was already told about")
	obj, _ := slow.Get("key")
	if !bytes.Equal(obj.Data, payload) {
		t.Errorf("the detached copy holds %d bytes, want %d", len(obj.Data), len(payload))
	}
}
