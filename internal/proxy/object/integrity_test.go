// -------------------------------------------------------------------------------
// Integrity Verification Tests
//
// Author: Alex Freidah
//
// Tests for read-time verification and the disposal of copies that fail it.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// writtenBytes is what a client stored, and rottedBytes is what the backend
// later returns for a copy that no longer matches the stored hash.
const (
	writtenBytes = "the bytes that were written"
	rottedBytes  = "these are not the bytes that were written"
)

// hashOf is the expected-digest helper these tests compare against. Written
// out rather than reusing production code so a bug in the hashing under test
// cannot mask itself by producing the same wrong answer on both sides.
func hashOf(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// verifyOnRead is the integrity config that turns on read-time verification.
func verifyOnRead() *config.IntegrityConfig {
	return &config.IntegrityConfig{Enabled: true, VerifyOnRead: true}
}

// -------------------------------------------------------------------------
// CORRUPTED COPY DISPOSAL
// -------------------------------------------------------------------------

// TestDropCorruptedLocation_RemovesRowAndCachedLocation covers the row the
// read path drops when a copy fails verification. Leaving it behind lets the
// replicator count a copy whose bytes were just deleted, so the object stays
// below its replication factor instead of being rebuilt.
func TestDropCorruptedLocation_RemovesRowAndCachedLocation(t *testing.T) {
	t.Parallel()

	const key, beName = "bucket/corrupt.txt", "b1"
	loc := &core.ObjectLocation{ObjectKey: key, BackendName: beName, StorageKey: key}

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), loc, 0, reasonIntegrityFailed).
		Return(integrityRows(key, beName), nil).Times(1)
	store.EXPECT().CompleteCleanupItems(gomock.Any(), gomock.Len(1)).Return([]int64{1}, nil).Times(1)
	storetest.Permissive(store)

	be := seededBackend(t, key)
	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
	f.cache.Set(key, beName)

	f.dropCorruptedLocation(context.Background(), loc)

	if cached, ok := f.cache.Get(key); ok {
		t.Errorf("location cache still resolves %q to %q after the copy was discarded", key, cached)
	}
	if be.HasCopyOf(key) {
		t.Error("the discarded copy's bytes are still on the backend")
	}
}

// TestDropCorruptedLocation_LeavesReplacedCopy covers a client overwrite that
// lands while the corrupted copy is being read: the row now names the newer
// write, so the delete is refused and neither the row, the cache entry nor any
// bytes are touched. The overwrite already queued the old bytes itself.
func TestDropCorruptedLocation_LeavesReplacedCopy(t *testing.T) {
	t.Parallel()

	const key, beName = "bucket/corrupt.txt", "b1"
	loc := &core.ObjectLocation{ObjectKey: key, BackendName: beName, StorageKey: key}

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), loc, 0, reasonIntegrityFailed).Return(nil, core.ErrCopyChanged).Times(1)
	storetest.Permissive(store)

	be := seededBackend(t, key)
	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
	f.cache.Set(key, beName)

	f.dropCorruptedLocation(context.Background(), loc)

	if _, ok := f.cache.Get(key); !ok {
		t.Errorf("location cache dropped %q for a copy that was not removed", key)
	}
	if !be.HasCopyOf(key) {
		t.Error("bytes deleted although the copy was never removed")
	}
}

// integrityRows returns the cleanup row the store queues for a copy of key on
// beName discarded as corrupt.
func integrityRows(key, beName string) []core.CleanupItem {
	return []core.CleanupItem{{
		ID: 1, BackendName: beName, ObjectKey: key, StorageKey: key, Reason: reasonIntegrityFailed, SizeBytes: 3,
	}}
}

// seededBackend returns an in-memory backend already holding bytes at key.
func seededBackend(t *testing.T, key string) *backendtest.InMemory {
	t.Helper()
	be := backendtest.NewInMemory()
	if _, err := be.PutObject(context.Background(), key, strings.NewReader("rot"), 3, "text/plain", nil); err != nil {
		t.Fatalf("seed backend: %v", err)
	}
	return be
}

// noDeleteStore returns a store mock that fails the test on any ledger delete,
// which is how every test of a copy that must survive asserts it.
func noDeleteStore(t *testing.T) *storetest.MockMetadataStore {
	t.Helper()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	return store
}

// TestDropCorruptedLocation_KeepsCacheWhenDeleteFails asserts the cache entry
// survives a failed delete. Evicting it would advertise a removal that did not
// happen, sending the next reader to the store to be told the copy is still
// there.
func TestDropCorruptedLocation_KeepsCacheWhenDeleteFails(t *testing.T) {
	t.Parallel()

	const key, beName = "bucket/corrupt.txt", "b1"

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), 0, reasonIntegrityFailed).
		Return(nil, errors.New("ledger unavailable")).Times(1)
	storetest.Permissive(store)

	be := backendtest.NewInMemory()
	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
	f.cache.Set(key, beName)

	f.dropCorruptedLocation(context.Background(), &core.ObjectLocation{ObjectKey: key, BackendName: beName})

	if _, ok := f.cache.Get(key); !ok {
		t.Errorf("location cache dropped %q even though the ledger delete failed", key)
	}
}

// -------------------------------------------------------------------------
// READ-TIME VERIFICATION
// -------------------------------------------------------------------------

// TestMaybeWrapIntegrityReader_DiscardsCopyOnMismatch drives the read-path
// detector: the body is wrapped, the mismatch surfaces when the client reads
// it to EOF, and the bad copy loses both its bytes and its ledger row.
func TestMaybeWrapIntegrityReader_DiscardsCopyOnMismatch(t *testing.T) {
	t.Parallel()

	const key, beName = "bucket/rotted.txt", "b1"

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), 0, reasonIntegrityFailed).
		Return(integrityRows(key, beName), nil).Times(1)
	storetest.Permissive(store)

	be := seededBackend(t, key)
	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
	f.SetIntegrityConfig(verifyOnRead())

	r := &backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader(rottedBytes)),
		Size: int64(len(rottedBytes)),
	}
	loc := &core.ObjectLocation{
		ObjectKey:   key,
		BackendName: beName,
		ContentHash: hashOf([]byte(writtenBytes)),
	}

	f.maybeWrapIntegrityReader(context.Background(), r, loc, key, beName, false)

	// The client still receives every byte; verification lands at EOF.
	if _, err := io.ReadAll(r.Body); err != nil {
		t.Fatalf("reading wrapped body: %v", err)
	}
	if be.HasCopyOf(key) {
		t.Error("the corrupt copy's bytes should be deleted once the read completes")
	}
	if err := r.Body.Close(); err != nil {
		t.Fatalf("closing wrapped body: %v", err)
	}
}

// TestMaybeWrapIntegrityReader_GoodCopySurvivesFullRead verifies a complete
// read of a copy that matches its stored hash leaves the copy alone.
func TestMaybeWrapIntegrityReader_GoodCopySurvivesFullRead(t *testing.T) {
	t.Parallel()

	const key, beName = "bucket/healthy.txt", "b1"
	store := noDeleteStore(t)
	storetest.Permissive(store)

	be := seededBackend(t, key)
	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
	f.SetIntegrityConfig(verifyOnRead())

	r := &backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader(writtenBytes)),
		Size: int64(len(writtenBytes)),
	}
	loc := &core.ObjectLocation{ObjectKey: key, BackendName: beName, ContentHash: hashOf([]byte(writtenBytes))}

	f.maybeWrapIntegrityReader(context.Background(), r, loc, key, beName, false)

	if _, err := io.ReadAll(r.Body); err != nil {
		t.Fatalf("reading wrapped body: %v", err)
	}
	if err := r.Body.Close(); err != nil {
		t.Fatalf("closing wrapped body: %v", err)
	}
	if !be.HasCopyOf(key) {
		t.Error("a healthy full read deleted the copy")
	}
}

// TestMaybeWrapIntegrityReader_IncompleteReadKeepsCopy is the regression test
// for downloads that stop early. A client disconnect or a backend stream that
// fails partway hashes only a prefix, which can never match the stored hash,
// so comparing it would destroy a healthy copy.
func TestMaybeWrapIntegrityReader_IncompleteReadKeepsCopy(t *testing.T) {
	t.Parallel()

	const key, beName = "bucket/interrupted.txt", "b1"
	cases := []struct {
		name string
		body io.Reader
		read func(io.ReadCloser) error
	}{
		{
			name: "client disconnect",
			body: strings.NewReader(writtenBytes),
			read: func(rc io.ReadCloser) error {
				_, err := rc.Read(make([]byte, 4))
				return err
			},
		},
		{
			name: "backend stream error",
			body: io.MultiReader(strings.NewReader(writtenBytes[:4]), iotest.ErrReader(context.DeadlineExceeded)),
			read: func(rc io.ReadCloser) error {
				if _, err := io.ReadAll(rc); !errors.Is(err, context.DeadlineExceeded) {
					return fmt.Errorf("read error = %w, want a deadline", err)
				}
				return nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := noDeleteStore(t)
			storetest.Permissive(store)

			be := seededBackend(t, key)
			f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
			f.SetIntegrityConfig(verifyOnRead())

			r := &backend.GetObjectResult{Body: io.NopCloser(tc.body), Size: int64(len(writtenBytes))}
			loc := &core.ObjectLocation{ObjectKey: key, BackendName: beName, ContentHash: hashOf([]byte(writtenBytes))}
			f.maybeWrapIntegrityReader(context.Background(), r, loc, key, beName, false)

			if err := tc.read(r.Body); err != nil {
				t.Fatalf("read: %v", err)
			}
			if err := r.Body.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if !be.HasCopyOf(key) {
				t.Error("an interrupted read deleted a healthy copy")
			}
		})
	}
}

// TestMaybeWrapIntegrityReader_LeavesBodyAlone covers the cases where no
// verification is possible or wanted, so the response body must pass through
// untouched rather than be silently wrapped.
func TestMaybeWrapIntegrityReader_LeavesBodyAlone(t *testing.T) {
	t.Parallel()

	const key, beName = "bucket/plain.txt", "b1"
	hashed := &core.ObjectLocation{ObjectKey: key, BackendName: beName, ContentHash: "abc"}

	cases := []struct {
		name string
		cfg  *config.IntegrityConfig
		loc  *core.ObjectLocation
	}{
		{"no integrity config", nil, hashed},
		{"integrity disabled", &config.IntegrityConfig{Enabled: false, VerifyOnRead: true}, hashed},
		{"verify on read off", &config.IntegrityConfig{Enabled: true, VerifyOnRead: false}, hashed},
		{"no stored hash", verifyOnRead(), &core.ObjectLocation{ObjectKey: key, BackendName: beName}},
		{"no location", verifyOnRead(), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := noDeleteStore(t)
			storetest.Permissive(store)

			be := backendtest.NewInMemory()
			f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
			f.SetIntegrityConfig(tc.cfg)

			body := io.NopCloser(strings.NewReader("whatever"))
			r := &backend.GetObjectResult{Body: body, Size: 8}

			f.maybeWrapIntegrityReader(context.Background(), r, tc.loc, key, beName, false)

			if r.Body != body {
				t.Error("response body was wrapped when no verification was possible")
			}
		})
	}
}

// -------------------------------------------------------------------------
// GET PATH
// -------------------------------------------------------------------------

// getFleet returns a fleet with verification on read whose only backend holds
// stored at key, and whose ledger row for key carries the hash of written.
func getFleet(t *testing.T, store *storetest.MockMetadataStore, key string, stored, written []byte) (*fleet, *backendtest.InMemory) {
	t.Helper()
	const beName = "b1"
	store.EXPECT().GetAllObjectLocations(gomock.Any(), key).Return([]core.ObjectLocation{{
		ObjectKey:   key,
		BackendName: beName,
		SizeBytes:   int64(len(stored)),
		ContentHash: hashOf(written),
	}}, nil).AnyTimes()
	storetest.Permissive(store)

	be := backendtest.NewInMemory()
	if _, err := be.PutObject(context.Background(), key, bytes.NewReader(stored), int64(len(stored)), "text/plain", nil); err != nil {
		t.Fatalf("seed backend: %v", err)
	}
	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
	f.SetIntegrityConfig(verifyOnRead())
	return f, be
}

// TestGet_RangedReadDoesNotVerifyOrDestroyCopy is the regression test for a
// data-loss bug: the stored hash covers the whole object, so a range could
// never match it, and the mismatch handler deletes the copy and its ledger
// row. A client doing ranged reads therefore destroyed one healthy copy per
// request, and the bytes it received were correct the whole time.
func TestGet_RangedReadDoesNotVerifyOrDestroyCopy(t *testing.T) {
	t.Parallel()
	const key = "bucket/ranged.txt"
	full := []byte("0123456789abcdefghijklmnopqrstuvwxyz")

	f, be := getFleet(t, noDeleteStore(t), key, full, full)

	res, err := f.GetObject(context.Background(), key, "bytes=0-9")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	if !bytes.Equal(got, full[:10]) {
		t.Errorf("ranged GET returned %q, want %q", got, full[:10])
	}
	if !be.HasCopyOf(key) {
		t.Error("a healthy ranged GET deleted the object from the backend")
	}
}

// TestGet_DisconnectedReadKeepsCopy covers a client that closes a whole-object
// GET partway: the bytes hashed so far are a prefix, so the healthy copy must
// survive.
func TestGet_DisconnectedReadKeepsCopy(t *testing.T) {
	t.Parallel()
	const key = "bucket/disconnect.txt"

	f, be := getFleet(t, noDeleteStore(t), key, []byte(writtenBytes), []byte(writtenBytes))

	res, err := f.GetObject(context.Background(), key, "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if _, err := res.Body.Read(make([]byte, 4)); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	if !be.HasCopyOf(key) {
		t.Error("a GET closed partway deleted a healthy copy")
	}
}

// TestGet_FullReadStillVerifies keeps the guard honest: skipping verification
// for a partial response must not switch it off for a whole-object read, which
// is the case the stored hash can actually validate.
func TestGet_FullReadStillVerifies(t *testing.T) {
	t.Parallel()
	const key = "bucket/rotted-full.txt"

	deleted := make(chan struct{}, 1)
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), 0, reasonIntegrityFailed).
		DoAndReturn(func(context.Context, *core.ObjectLocation, int, string) ([]core.CleanupItem, error) {
			deleted <- struct{}{}
			return nil, nil
		}).Times(1)
	f, _ := getFleet(t, store, key, []byte(rottedBytes), []byte(writtenBytes))

	res, err := f.GetObject(context.Background(), key, "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	select {
	case <-deleted:
	default:
		t.Error("a corrupt whole-object read did not discard the copy")
	}
}
