// -------------------------------------------------------------------------------
// Integrity Verification Tests
//
// Author: Alex Freidah
//
// Tests for content hashing and streaming verification.
// -------------------------------------------------------------------------------

package object

import (
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
	store.EXPECT().DeleteObjectLocation(gomock.Any(), key, beName).Return(nil).Times(1)
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
		Return(errors.New("ledger unavailable")).Times(1)
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
	store.EXPECT().DeleteObjectLocation(gomock.Any(), key, beName).Return(nil).Times(1)
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

	f.maybeWrapIntegrityReader(context.Background(), r, loc, key, beName, be)

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

			f.maybeWrapIntegrityReader(context.Background(), r, loc, key, beName, be)

			if r.Body != body {
				t.Error("response body was wrapped when no verification was possible")
			}
		})
	}
}
