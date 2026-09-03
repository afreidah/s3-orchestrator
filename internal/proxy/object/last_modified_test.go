// -------------------------------------------------------------------------------
// Object Manager - Last-Modified Resolution Tests
//
// Author: Alex Freidah
//
// Covers which timestamp a read answers with. The stored CreatedAt wins
// whenever a location row is present, including when the backend reported a
// time of its own: the stored value is the object's write time and is the same
// on every copy, where a backend reports when it received the copy that
// happened to answer. Preferring the backend is what let an unmodified object
// change its Last-Modified on failover.
//
// Only the degraded broadcast path, serving with the database down and no row
// to consult, keeps the backend's value - and a backend that reports nothing
// there still leaves it zero, which is the one case the transport drops.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newLMObjectCache builds a small object cache for the cache-hit tests.
func newLMObjectCache(t *testing.T) objcache.ObjectCache {
	t.Helper()
	c, err := objcache.NewMemoryCache(objcache.MemoryConfig{
		MaxSize:       1 << 20,
		MaxObjectSize: 1 << 20,
		TTL:           time.Minute,
	})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	return c
}

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// lmCreatedAt is the location row's creation time, used as the fallback.
var lmCreatedAt = time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)

// lmBackendTime is a backend-reported modification time, distinct from the
// fallback so a test can tell which one the read path kept.
var lmBackendTime = time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestHeadObject_ZeroLastModified_FallsBackToCreatedAt asserts a HEAD whose
// backend reports no modification time reports the location's CreatedAt.
func TestHeadObject_ZeroLastModified_FallsBackToCreatedAt(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	// PutObject leaves LastModified zero, standing in for a backend that
	// returns nil.
	_, _ = be.PutObject(context.Background(), "key", bytes.NewReader([]byte("headme")), 6, "application/json", nil)

	store := locationsStore(t, []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1", CreatedAt: lmCreatedAt}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if !result.LastModified.Equal(lmCreatedAt) {
		t.Errorf("last-modified = %v, want fallback %v", result.LastModified, lmCreatedAt)
	}
}

// TestHeadObject_StoredLastModifiedWinsOverBackend asserts the stored write
// time is reported even when the backend supplied one of its own. Two copies
// of an unmodified object report different backend times, so answering with
// the backend's makes Last-Modified depend on which one served the read.
func TestHeadObject_StoredLastModifiedWinsOverBackend(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Put("key", &backendtest.Object{
		Data: []byte("headme"), ContentType: "application/json", ETag: `"6"`, LastModified: lmBackendTime,
	})

	store := locationsStore(t, []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1", CreatedAt: lmCreatedAt}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if !result.LastModified.Equal(lmCreatedAt) {
		t.Errorf("last-modified = %v, want stored value %v (the backend's must not win)", result.LastModified, lmCreatedAt)
	}
}

// TestHeadObject_ZeroLastModified_NoLocationStaysZero asserts the degraded
// broadcast path (no location row) leaves a zero time untouched - there is no
// CreatedAt to borrow.
func TestHeadObject_ZeroLastModified_NoLocationStaysZero(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "key", bytes.NewReader([]byte("head")), 4, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject broadcast should succeed: %v", err)
	}
	if !result.LastModified.IsZero() {
		t.Errorf("last-modified = %v, want zero (no location row to fall back to)", result.LastModified)
	}
}

// TestGetObject_ZeroLastModified_FallsBackToCreatedAt asserts a GET whose
// backend reports no modification time reports the location's CreatedAt.
func TestGetObject_ZeroLastModified_FallsBackToCreatedAt(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "key", bytes.NewReader([]byte("hello")), 5, "text/plain", nil)

	store := locationsStore(t, []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1", CreatedAt: lmCreatedAt}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	if !result.LastModified.Equal(lmCreatedAt) {
		t.Errorf("last-modified = %v, want fallback %v", result.LastModified, lmCreatedAt)
	}
}

// TestGetObject_StoredLastModifiedWinsOverBackend asserts the GET read path
// answers with the stored write time rather than the backend's.
func TestGetObject_StoredLastModifiedWinsOverBackend(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Put("key", &backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"5"`, LastModified: lmBackendTime,
	})

	store := locationsStore(t, []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1", CreatedAt: lmCreatedAt}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	if !result.LastModified.Equal(lmCreatedAt) {
		t.Errorf("last-modified = %v, want stored value %v (the backend's must not win)", result.LastModified, lmCreatedAt)
	}
}

// TestGetObject_CacheHit_CarriesFallbackLastModified asserts the resolved
// timestamp survives a round-trip through the object cache: the first GET
// resolves a zero backend time to CreatedAt and populates the cache, and the
// cache-hit GET reports the same value rather than dropping it.
func TestGetObject_CacheHit_CarriesFallbackLastModified(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "key", bytes.NewReader([]byte("hello")), 5, "text/plain", nil)

	store := locationsStore(t, []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1", CreatedAt: lmCreatedAt}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, &fleetOpts{ObjectCache: newLMObjectCache(t)})

	// First GET reads from the backend and must be drained to completion so
	// the cache-tee finalizes and stores the entry.
	r1, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("first GetObject: %v", err)
	}
	_, _ = io.ReadAll(r1.Body)
	_ = r1.Body.Close()

	r2, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("second GetObject (cache hit): %v", err)
	}
	defer func() { _ = r2.Body.Close() }()
	if !r2.LastModified.Equal(lmCreatedAt) {
		t.Errorf("cache-hit last-modified = %v, want fallback %v", r2.LastModified, lmCreatedAt)
	}
}

// TestGetObject_CacheHit_CarriesStoredLastModified asserts the cache stores
// the resolved timestamp, not the backend's: a hit must answer with the same
// value the miss did, or the reported time would depend on cache residency.
func TestGetObject_CacheHit_CarriesStoredLastModified(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Put("key", &backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"5"`, LastModified: lmBackendTime,
	})

	store := locationsStore(t, []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1", CreatedAt: lmCreatedAt}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, &fleetOpts{ObjectCache: newLMObjectCache(t)})

	r1, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("first GetObject: %v", err)
	}
	_, _ = io.ReadAll(r1.Body)
	_ = r1.Body.Close()

	r2, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("second GetObject (cache hit): %v", err)
	}
	defer func() { _ = r2.Body.Close() }()
	if !r2.LastModified.Equal(lmCreatedAt) {
		t.Errorf("cache-hit last-modified = %v, want stored value %v", r2.LastModified, lmCreatedAt)
	}
}

// TestGetObject_ZeroLastModified_NoLocationStaysZero asserts the degraded
// broadcast GET (no location row) leaves a zero time untouched.
func TestGetObject_ZeroLastModified_NoLocationStaysZero(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "key", bytes.NewReader([]byte("broadcast")), 9, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject broadcast should succeed: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	if !result.LastModified.IsZero() {
		t.Errorf("last-modified = %v, want zero (no location row to fall back to)", result.LastModified)
	}
}
