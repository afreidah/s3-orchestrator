// -------------------------------------------------------------------------------
// Ops - Paged Pass Tests
//
// Author: Alex Freidah
//
// Covers the passes that walk a listing by cursor against listings that shrink
// as rows are processed, which is the case offset paging gets wrong.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
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

// ListCopiesByForm lists up to limit unhashed copies after the cursor. It
// serves only the backfill's filter, managed copies without a hash, so a pass
// that asked for anything else finds nothing to do.
func (b *backlog) ListCopiesByForm(_ context.Context, f *core.FormFilter, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
	if !f.Without.Has(core.StoredHashed) || !f.ManagedOnly {
		return nil, nil
	}
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
		DoAndReturn(func(_ context.Context, locs []core.ObjectLocation, observer progress.Observer) batch.Summary {
			var sum batch.Summary
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
	locs []core.ObjectLocation
}

// namedKey derives a distinct master key from name, so tests can refer to
// keys by name and to their IDs by keyIDOf.
func namedKey(name string) []byte {
	sum := sha256.Sum256([]byte(name))
	return sum[:]
}

// keyIDOf returns the ID the key named name records DEKs under.
// It is the fingerprint of namedKey(name).
func keyIDOf(name string) string {
	return encryption.Fingerprint(namedKey(name))
}

// keyedEncryptor builds an encryptor whose primary key is primary and which can
// still unwrap DEKs sealed under each of previous. Each name gets its own
// master key.
func keyedEncryptor(t *testing.T, primary string, previous ...string) *encryption.Encryptor {
	t.Helper()
	provider := func(name string) encryption.KeyProvider {
		p, err := encryption.NewStaticKeyProvider(namedKey(name))
		if err != nil {
			t.Fatalf("NewStaticKeyProvider(%s): %v", name, err)
		}
		return p
	}
	prev := make([]encryption.KeyProvider, 0, len(previous))
	for _, name := range previous {
		prev = append(prev, provider(name))
	}
	enc, err := encryption.NewEncryptor(encryption.NewMultiKeyProvider(provider(primary), prev), 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

// rotateWith runs one rotation over store with enc, of oldKeyID or, when it
// is empty, of every stale key.
func rotateWith(t *testing.T, enc *encryption.Encryptor, store *rotatingStore, oldKeyID string) RotateKeyResult {
	t.Helper()
	svc := NewEncryption(EncryptionDeps{
		Encryptor: enc,
		Store:     store,
		Runtime:   opstest.NewMockRuntimeOps(gomock.NewController(t)),
		Mover:     &fakeMover{},
	})
	res, err := svc.RotateKey(context.Background(), oldKeyID, oldKeyID == "")
	if err != nil {
		t.Fatalf("RotateKey(%q): %v", oldKeyID, err)
	}
	return res
}

// assertAllUnder fails unless every copy in store is under keyID.
func assertAllUnder(t *testing.T, store *rotatingStore, keyID string) {
	t.Helper()
	for i := range store.locs {
		loc := &store.locs[i]
		if loc.KeyID != keyID {
			t.Fatalf("copy %s is under %q, want %q", loc.ObjectKey, loc.KeyID, keyID)
		}
	}
}

// ListCopiesByForm lists up to limit encrypted copies after the cursor that
// match the filter's key id, or that are not under its excluded key id.
func (s *rotatingStore) ListCopiesByForm(_ context.Context, f *core.FormFilter, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
	if !f.With.Has(core.StoredEncrypted) {
		return nil, nil
	}
	var out []core.ObjectLocation
	for i := range s.locs {
		loc := &s.locs[i]
		if len(out) == limit {
			break
		}
		matches := (f.KeyID == "" || loc.KeyID == f.KeyID) && (f.ExceptKeyID == "" || loc.KeyID != f.ExceptKeyID)
		if matches && cursorAfter(loc.ObjectKey, loc.BackendName, after) {
			out = append(out, *loc)
		}
	}
	return out, nil
}

// UpdateCopyMetadata records the copy's re-wrapped key and its new key id, and
// reports core.ErrCopyChanged when no copy matches.
func (s *rotatingStore) UpdateCopyMetadata(_ context.Context, loc *core.ObjectLocation, p *core.CopyPatch) error {
	for i := range s.locs {
		if s.locs[i].ObjectKey == loc.ObjectKey && s.locs[i].BackendName == loc.BackendName && s.locs[i].StorageKey == loc.StorageKey {
			s.locs[i].EncryptionKey = p.EncryptionKey
			s.locs[i].KeyID = p.KeyID
			return nil
		}
	}
	return core.ErrCopyChanged
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
	if res.Succeeded != 25 || !res.Done {
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
		store.locs = append(store.locs, core.ObjectLocation{
			ObjectKey: fmt.Sprintf("k-%04d", i), BackendName: "b1", EncryptionKey: keyData, KeyID: keyID,
		})
	}

	if res := rotateWith(t, keyedEncryptor(t, "key-b", "key-a"), store, keyIDOf("key-a")); res.Rotated != copies || res.Failed != 0 {
		t.Errorf("first rotation = %+v, want all %d rotated", res, copies)
	}
	assertAllUnder(t, store, keyIDOf("key-b"))

	if res := rotateWith(t, keyedEncryptor(t, "key-c", "key-b"), store, keyIDOf("key-b")); res.Rotated != copies || res.Failed != 0 {
		t.Errorf("second rotation = %+v, want all %d rotated", res, copies)
	}
	assertAllUnder(t, store, keyIDOf("key-c"))
}

// TestRotateKey_AllStaleMovesEveryOtherID verifies an all-stale rotation
// across pages re-wraps copies under positional IDs from earlier releases and
// under old fingerprints, and leaves copies already under the current key.
func TestRotateKey_AllStaleMovesEveryOtherID(t *testing.T) {
	t.Parallel()
	const copies = rotateBatchSize + 5
	oldData, _, _ := sealed(t, keyedEncryptor(t, "key-a"), []byte("hello world"))
	currentData, currentID, _ := sealed(t, keyedEncryptor(t, "key-b"), []byte("hello world"))
	store := &rotatingStore{}
	for i := range copies {
		keyID := "config-0"
		if i%2 == 1 {
			keyID = keyIDOf("key-a")
		}
		store.locs = append(store.locs, core.ObjectLocation{
			ObjectKey: fmt.Sprintf("k-%04d", i), BackendName: "b1", EncryptionKey: oldData, KeyID: keyID,
		})
	}
	store.locs = append(store.locs, core.ObjectLocation{
		ObjectKey: "z-current", BackendName: "b1", EncryptionKey: currentData, KeyID: currentID,
	})

	if res := rotateWith(t, keyedEncryptor(t, "key-b", "key-a"), store, ""); res.Rotated != copies || res.Failed != 0 || res.Total != copies {
		t.Errorf("rotation = %+v, want all %d stale copies rotated and the current one left out", res, copies)
	}
	assertAllUnder(t, store, currentID)
}
