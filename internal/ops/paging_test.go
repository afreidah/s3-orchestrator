// -------------------------------------------------------------------------------
// Ops - Cursor Paging Tests
//
// Author: Alex Freidah
//
// Covers walkPages and the passes built on it against listings that shrink as
// rows are processed, which is the case offset paging gets wrong.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// backlog is an UnhashedLister over an in-memory set of unhashed copies on one
// backend. Hashing a copy removes it, as a stored hash takes a row out of the
// real listing.
type backlog struct {
	keys []string
}

// newBacklog builds a backlog of n copies with keys in cursor order.
func newBacklog(n int) *backlog {
	b := &backlog{}
	for i := range n {
		b.keys = append(b.keys, fmt.Sprintf("k-%03d", i))
	}
	return b
}

// GetObjectsWithoutHash lists up to limit unhashed copies after the cursor.
func (b *backlog) GetObjectsWithoutHash(_ context.Context, limit int, after core.Cursor, _ string) ([]core.ObjectLocation, error) {
	var out []core.ObjectLocation
	for _, k := range b.keys {
		if len(out) == limit {
			break
		}
		if cursorAfter(k, "b1", after) {
			out = append(out, core.ObjectLocation{ObjectKey: k, BackendName: "b1"})
		}
	}
	return out, nil
}

// hash takes a copy out of the backlog.
func (b *backlog) hash(key string) {
	b.keys = slices.DeleteFunc(b.keys, func(k string) bool { return k == key })
}

// cursorAfter reports whether (key, backend) sorts after the cursor.
func cursorAfter(key, backend string, after core.Cursor) bool {
	return key > after.ObjectKey || (key == after.ObjectKey && backend > after.BackendName)
}

// backlogHasher is a scrubber whose HashCopies hashes every copy in the page
// except the ones unreadable names, which it skips and leaves in the backlog.
func backlogHasher(t *testing.T, b *backlog, unreadable func(key string) bool) *opstest.MockScrubberOps {
	t.Helper()
	scrubber := opstest.NewMockScrubberOps(gomock.NewController(t))
	scrubber.EXPECT().HashCopies(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, locs []core.ObjectLocation, observer progress.Observer) worker.WorkSummary {
			var sum worker.WorkSummary
			for i := range locs {
				key := locs[i].ObjectKey
				sum.Attempted++
				if unreadable != nil && unreadable(key) {
					sum.Skipped++
					progress.Track(observer, key, func() string { return progress.StatusUnreadable })
					continue
				}
				b.hash(key)
				sum.Succeeded++
				progress.Track(observer, key, func() string { return progress.StatusOK })
			}
			return sum
		}).AnyTimes()
	return scrubber
}

// rotatingStore is an EncryptionStore over in-memory encrypted copies. A
// rotation records the new key id, which takes the copy out of the listing for
// the old one.
type rotatingStore struct {
	emptyEncAdmin
	locs []core.EncryptedLocation
}

// keyedEncryptor builds an encryptor whose primary key is primary and which can
// still unwrap DEKs sealed under each of previous. Each key id gets its own
// master key.
func keyedEncryptor(t *testing.T, primary string, previous ...string) *encryption.Encryptor {
	t.Helper()
	provider := func(id string) encryption.KeyProvider {
		sum := sha256.Sum256([]byte(id))
		p, err := encryption.NewConfigKeyProvider(base64.StdEncoding.EncodeToString(sum[:]), id)
		if err != nil {
			t.Fatalf("NewConfigKeyProvider(%s): %v", id, err)
		}
		return p
	}
	prev := make([]encryption.KeyProvider, 0, len(previous))
	for _, id := range previous {
		prev = append(prev, provider(id))
	}
	enc, err := encryption.NewEncryptor(encryption.NewMultiKeyProvider(provider(primary), prev), 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

// rotateWith runs one rotation of oldKeyID over store with enc.
func rotateWith(t *testing.T, enc *encryption.Encryptor, store *rotatingStore, oldKeyID string) RotateKeyResult {
	t.Helper()
	svc := NewEncryption(EncryptionDeps{
		Encryptor: enc,
		Store:     store,
		Runtime:   opstest.NewMockRuntimeOps(gomock.NewController(t)),
		Usage:     opstest.NewMockUsageGate(gomock.NewController(t)),
	})
	res, err := svc.RotateKey(context.Background(), oldKeyID)
	if err != nil {
		t.Fatalf("RotateKey(%s): %v", oldKeyID, err)
	}
	return res
}

// assertAllUnder fails unless every copy in store is under keyID.
func assertAllUnder(t *testing.T, store *rotatingStore, keyID string) {
	t.Helper()
	for _, loc := range store.locs {
		if loc.KeyID != keyID {
			t.Fatalf("copy %s is under %q, want %q", loc.ObjectKey, loc.KeyID, keyID)
		}
	}
}

// ListEncryptedLocations lists up to limit copies under keyID after the cursor.
func (s *rotatingStore) ListEncryptedLocations(_ context.Context, keyID string, limit int, after core.Cursor) ([]core.EncryptedLocation, error) {
	var out []core.EncryptedLocation
	for _, loc := range s.locs {
		if len(out) == limit {
			break
		}
		if loc.KeyID == keyID && cursorAfter(loc.ObjectKey, loc.BackendName, after) {
			out = append(out, loc)
		}
	}
	return out, nil
}

// UpdateEncryptionKey records the copy's re-wrapped key and its new key id.
func (s *rotatingStore) UpdateEncryptionKey(_ context.Context, objectKey, backendName string, keyData []byte, newKeyID string) error {
	for i := range s.locs {
		if s.locs[i].ObjectKey == objectKey && s.locs[i].BackendName == backendName {
			s.locs[i].EncryptionKey = keyData
			s.locs[i].KeyID = newKeyID
		}
	}
	return nil
}

// -------------------------------------------------------------------------
// WALK PAGES
// -------------------------------------------------------------------------

// TestWalkPages_AdvancesTheCursorPastEachPage verifies each page is listed
// after the last row of the one before, and a short page ends the walk.
func TestWalkPages_AdvancesTheCursorPastEachPage(t *testing.T) {
	t.Parallel()
	rows := []string{"a", "b", "c", "d", "e"}
	var cursors []string
	list := func(_ context.Context, limit int, after core.Cursor) ([]string, error) {
		cursors = append(cursors, after.ObjectKey)
		var out []string
		for _, r := range rows {
			if r > after.ObjectKey && len(out) < limit {
				out = append(out, r)
			}
		}
		return out, nil
	}
	var seen []string
	stopped, err := walkPages(context.Background(), fixedPage(2), list,
		func(r string) core.Cursor { return core.Cursor{ObjectKey: r} },
		func(_ context.Context, page []string) (bool, error) {
			seen = append(seen, page...)
			return false, nil
		})
	if err != nil || stopped {
		t.Fatalf("walkPages = (%v, %v), want (false, nil)", stopped, err)
	}
	if !slices.Equal(seen, rows) {
		t.Errorf("seen = %v, want %v", seen, rows)
	}
	if want := []string{"", "b", "d"}; !slices.Equal(cursors, want) {
		t.Errorf("cursors = %v, want %v", cursors, want)
	}
}

// TestWalkPages_StopsWhenVisitAsks verifies a visit asking to stop ends the
// walk without listing another page, and is reported.
func TestWalkPages_StopsWhenVisitAsks(t *testing.T) {
	t.Parallel()
	var lists int
	list := func(context.Context, int, core.Cursor) ([]int, error) {
		lists++
		return []int{1, 2}, nil
	}
	stopped, err := walkPages(context.Background(), fixedPage(2), list,
		func(int) core.Cursor { return core.Cursor{} },
		func(context.Context, []int) (bool, error) { return true, nil })
	if err != nil || !stopped || lists != 1 {
		t.Errorf("walkPages = (%v, %v) after %d lists, want (true, nil) after 1", stopped, err, lists)
	}
}

// TestWalkPages_ReturnsListAndVisitErrors verifies a failure on either side
// ends the walk with that error.
func TestWalkPages_ReturnsListAndVisitErrors(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom")
	failingList := func(context.Context, int, core.Cursor) ([]int, error) { return nil, wantErr }
	okList := func(context.Context, int, core.Cursor) ([]int, error) { return []int{1}, nil }
	cursorOf := func(int) core.Cursor { return core.Cursor{} }
	noop := func(context.Context, []int) (bool, error) { return false, nil }
	failingVisit := func(context.Context, []int) (bool, error) { return false, wantErr }

	if _, err := walkPages(context.Background(), fixedPage(1), failingList, cursorOf, noop); !errors.Is(err, wantErr) {
		t.Errorf("list failure: err = %v, want %v", err, wantErr)
	}
	if _, err := walkPages(context.Background(), fixedPage(1), okList, cursorOf, failingVisit); !errors.Is(err, wantErr) {
		t.Errorf("visit failure: err = %v, want %v", err, wantErr)
	}
}

// -------------------------------------------------------------------------
// PASSES OVER A SHRINKING LISTING
// -------------------------------------------------------------------------

// TestBackfillChecksums_HashesEveryRowAcrossPages verifies a backfill over a
// backlog several pages long hashes every copy, even though each hashed copy
// leaves the listing it was read from.
func TestBackfillChecksums_HashesEveryRowAcrossPages(t *testing.T) {
	t.Parallel()
	b := newBacklog(25)
	svc := integrityWithBacklog(t, backlogHasher(t, b, nil), b)

	res, err := svc.BackfillChecksums(context.Background(), 10, 0, 0, "", nil)
	if err != nil {
		t.Fatalf("BackfillChecksums: %v", err)
	}
	if res.Processed != 25 || !res.Done {
		t.Errorf("res = %+v, want all 25 processed and the backlog drained", res)
	}
	if len(b.keys) != 0 {
		t.Errorf("unhashed = %v, want none left", b.keys)
	}
}

// TestRotateKey_RotatesEveryRowAcrossPages verifies rotations over more than
// one page re-wrap every copy, even though each rotated copy leaves the
// listing it was read from, and that a second rotation moves them again.
func TestRotateKey_RotatesEveryRowAcrossPages(t *testing.T) {
	t.Parallel()
	const copies = rotateBatchSize + 5
	keyData, keyID, _ := sealed(t, keyedEncryptor(t, "key-a"), []byte("hello world"))
	store := &rotatingStore{}
	for i := range copies {
		store.locs = append(store.locs, core.EncryptedLocation{
			ObjectKey: fmt.Sprintf("k-%04d", i), BackendName: "b1", EncryptionKey: keyData, KeyID: keyID,
		})
	}

	if res := rotateWith(t, keyedEncryptor(t, "key-b", "key-a"), store, "key-a"); res.Rotated != copies || res.Failed != 0 {
		t.Errorf("first rotation = %+v, want all %d rotated", res, copies)
	}
	assertAllUnder(t, store, "key-b")

	if res := rotateWith(t, keyedEncryptor(t, "key-c", "key-b"), store, "key-b"); res.Rotated != copies || res.Failed != 0 {
		t.Errorf("second rotation = %+v, want all %d rotated", res, copies)
	}
	assertAllUnder(t, store, "key-c")
}
