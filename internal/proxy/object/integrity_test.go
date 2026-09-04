// -------------------------------------------------------------------------------
// Integrity Verification Tests
//
// Author: Alex Freidah
//
// Tests for content hashing and streaming verification.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// hashOf is the expected-digest helper these tests compare against. Written
// out rather than reusing production code so a bug in the hashing under test
// cannot mask itself by producing the same wrong answer on both sides.
func hashOf(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// TestVerifyingReader_Match verifies the verifying reader match contract.
// Asserts that Verify should pass:.
func TestVerifyingReader_Match(t *testing.T) {
	t.Parallel()
	data := "test data for hashing"
	expected := hashOf([]byte(data))

	r := io.NopCloser(strings.NewReader(data))
	vr := NewVerifyingReader(r)
	_, _ = io.ReadAll(vr)

	if err := vr.Verify(expected); err != nil {
		t.Errorf("Verify should pass: %v", err)
	}
}

// TestVerifyingReader_Mismatch verifies the verifying reader mismatch path by exercising io.NopCloser, strings.NewReader, io.ReadAll.
func TestVerifyingReader_Mismatch(t *testing.T) {
	t.Parallel()
	r := io.NopCloser(strings.NewReader("actual data"))
	vr := NewVerifyingReader(r)
	_, _ = io.ReadAll(vr)

	if err := vr.Verify("0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Error("Verify should fail on hash mismatch")
	}
}

// TestVerifyingReader_EmptyExpected verifies the verifying reader empty expected contract.
// Asserts that Verify with empty expected should pass:.
func TestVerifyingReader_EmptyExpected(t *testing.T) {
	t.Parallel()
	r := io.NopCloser(strings.NewReader("any data"))
	vr := NewVerifyingReader(r)
	_, _ = io.ReadAll(vr)

	if err := vr.Verify(""); err != nil {
		t.Errorf("Verify with empty expected should pass: %v", err)
	}
}

// TestVerifyingReader_OnMismatchCallback verifies the verifying reader on mismatch callback path by exercising io.NopCloser, strings.NewReader, vr.SetVerification.
func TestVerifyingReader_OnMismatchCallback(t *testing.T) {
	t.Parallel()
	r := io.NopCloser(strings.NewReader("actual data"))
	vr := NewVerifyingReader(r)

	called := false
	vr.SetVerification("0000000000000000000000000000000000000000000000000000000000000000", func(expected, actual string) {
		called = true
	})

	_, _ = io.ReadAll(vr)
	_ = vr.Close()

	if !called {
		t.Error("onMismatch callback should have been called")
	}
}

// TestVerifyingReader_OnMatchNoCallback verifies the verifying reader on match no callback path by exercising io.NopCloser, strings.NewReader, vr.SetVerification.
func TestVerifyingReader_OnMatchNoCallback(t *testing.T) {
	t.Parallel()
	data := "matching data"
	expected := hashOf([]byte(data))

	r := io.NopCloser(strings.NewReader(data))
	vr := NewVerifyingReader(r)

	called := false
	vr.SetVerification(expected, func(expected, actual string) {
		called = true
	})

	_, _ = io.ReadAll(vr)
	_ = vr.Close()

	if called {
		t.Error("onMismatch callback should not be called on matching hash")
	}
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

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), key, beName).Return(int64(0), nil).Times(1)
	storetest.Permissive(store)

	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: backendtest.NewInMemory()}, nil)
	f.cache.Set(key, beName)

	f.dropCorruptedLocation(context.Background(), key, beName)

	if cached, ok := f.cache.Get(key); ok {
		t.Errorf("location cache still resolves %q to %q after the copy was discarded", key, cached)
	}
}

// TestDropCorruptedLocation_KeepsCacheWhenDeleteFails asserts the cache entry
// survives a failed delete. Evicting it would advertise a removal that did not
// happen, sending the next reader to the store to be told the copy is still
// there.
func TestDropCorruptedLocation_KeepsCacheWhenDeleteFails(t *testing.T) {
	t.Parallel()

	const key, beName = "bucket/corrupt.txt", "b1"

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), key, beName).
		Return(int64(0), errors.New("ledger unavailable")).Times(1)
	storetest.Permissive(store)

	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: backendtest.NewInMemory()}, nil)
	f.cache.Set(key, beName)

	f.dropCorruptedLocation(context.Background(), key, beName)

	if _, ok := f.cache.Get(key); !ok {
		t.Errorf("location cache dropped %q even though the ledger delete failed", key)
	}
}

// TestMaybeWrapIntegrityReader_DiscardsCopyOnMismatch drives the read-path
// detector: the body is wrapped, the mismatch surfaces when the client closes
// it, and the bad copy loses both its bytes and its ledger row.
func TestMaybeWrapIntegrityReader_DiscardsCopyOnMismatch(t *testing.T) {
	t.Parallel()

	const key, beName = "bucket/rotted.txt", "b1"
	stored := "these are not the bytes that were written"

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), key, beName).Return(int64(0), nil).Times(1)
	storetest.Permissive(store)

	be := backendtest.NewInMemory()
	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
	f.SetIntegrityConfig(&config.IntegrityConfig{Enabled: true, VerifyOnRead: true})

	r := &backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader(stored)),
		Size: int64(len(stored)),
	}
	loc := &core.ObjectLocation{
		ObjectKey:   key,
		BackendName: beName,
		ContentHash: hashOf([]byte("the bytes that were written")),
	}

	f.maybeWrapIntegrityReader(context.Background(), r, loc, key, beName, be, false)

	// The client still receives the bytes; verification lands on Close.
	if _, err := io.ReadAll(r.Body); err != nil {
		t.Fatalf("reading wrapped body: %v", err)
	}
	if err := r.Body.Close(); err != nil {
		t.Fatalf("closing wrapped body: %v", err)
	}
}

// TestMaybeWrapIntegrityReader_LeavesBodyAlone covers the cases where no
// verification is possible or wanted, so the response body must pass through
// untouched rather than be silently wrapped.
func TestMaybeWrapIntegrityReader_LeavesBodyAlone(t *testing.T) {
	t.Parallel()

	const key, beName = "bucket/plain.txt", "b1"

	cases := []struct {
		name string
		cfg  *config.IntegrityConfig
		hash string
	}{
		{"integrity disabled", &config.IntegrityConfig{Enabled: false, VerifyOnRead: true}, "abc"},
		{"verify on read off", &config.IntegrityConfig{Enabled: true, VerifyOnRead: false}, "abc"},
		{"no stored hash", &config.IntegrityConfig{Enabled: true, VerifyOnRead: true}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := storetest.NewMockMetadataStore(gomock.NewController(t))
			store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
			storetest.Permissive(store)

			be := backendtest.NewInMemory()
			f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
			f.SetIntegrityConfig(tc.cfg)

			body := io.NopCloser(strings.NewReader("whatever"))
			r := &backend.GetObjectResult{Body: body, Size: 8}
			loc := &core.ObjectLocation{ObjectKey: key, BackendName: beName, ContentHash: tc.hash}

			f.maybeWrapIntegrityReader(context.Background(), r, loc, key, beName, be, false)

			if r.Body != body {
				t.Error("response body was wrapped when no verification was possible")
			}
		})
	}
}

// TestGet_RangedReadDoesNotVerifyOrDestroyCopy is the regression test for a
// data-loss bug: the stored hash covers the whole object, so a range could
// never match it, and the mismatch handler deletes the copy and its ledger
// row. A client doing ranged reads therefore destroyed one healthy copy per
// request, and the bytes it received were correct the whole time.
func TestGet_RangedReadDoesNotVerifyOrDestroyCopy(t *testing.T) {
	t.Parallel()
	const key, beName = "bucket/ranged.txt", "b1"
	full := []byte("0123456789abcdefghijklmnopqrstuvwxyz")

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	// Not AnyTimes: any delete at all is the bug.
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), key).Return([]core.ObjectLocation{{
		ObjectKey:   key,
		BackendName: beName,
		SizeBytes:   int64(len(full)),
		ContentHash: hashOf(full),
	}}, nil).AnyTimes()
	storetest.Permissive(store)

	be := backendtest.NewInMemory()
	if _, err := be.PutObject(context.Background(), key, bytes.NewReader(full), int64(len(full)), "text/plain", nil); err != nil {
		t.Fatalf("seed backend: %v", err)
	}
	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
	f.SetIntegrityConfig(&config.IntegrityConfig{Enabled: true, VerifyOnRead: true})

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
	if !be.Has(key) {
		t.Error("a healthy ranged GET deleted the object from the backend")
	}
}

// TestGet_FullReadStillVerifies keeps the guard honest: skipping verification
// for a partial response must not switch it off for a whole-object read, which
// is the case the stored hash can actually validate.
func TestGet_FullReadStillVerifies(t *testing.T) {
	t.Parallel()
	const key, beName = "bucket/rotted-full.txt", "b1"
	stored := []byte("these are not the bytes that were written")

	deleted := make(chan struct{}, 1)
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), key, beName).
		DoAndReturn(func(context.Context, string, string) (int64, error) {
			deleted <- struct{}{}
			return 0, nil
		}).Times(1)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), key).Return([]core.ObjectLocation{{
		ObjectKey:   key,
		BackendName: beName,
		SizeBytes:   int64(len(stored)),
		ContentHash: hashOf([]byte("the bytes that were written")),
	}}, nil).AnyTimes()
	storetest.Permissive(store)

	be := backendtest.NewInMemory()
	if _, err := be.PutObject(context.Background(), key, bytes.NewReader(stored), int64(len(stored)), "text/plain", nil); err != nil {
		t.Fatalf("seed backend: %v", err)
	}
	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, nil)
	f.SetIntegrityConfig(&config.IntegrityConfig{Enabled: true, VerifyOnRead: true})

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
