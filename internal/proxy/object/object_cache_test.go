// -------------------------------------------------------------------------------
// Object Manager - Object Cache Read Tests
//
// Author: Alex Freidah
//
// The object cache is keyed by storage path, which no write reuses, so a GET
// only hits when one of the key's current rows names a cached path. These
// cover hits on any copy's path, an overwrite missing the old path, a lookup
// failure never hitting, a miss filling the cache under the copy that served,
// and ranged reads bypassing the cache.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// cacheKey is the object key under test, and the paths are where successive
// writes of it store their bytes.
const (
	cacheKey     = "key"
	cachePathOld = "key!w1"
	cachePathNew = "key!w2"
	cacheType    = "text/plain"
	cachedBody   = "cached"
	freshBody    = "fresh"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// newReadCache builds a memory cache large enough for every read here.
func newReadCache(t *testing.T) *objcache.MemoryCache {
	t.Helper()
	c, err := objcache.NewMemoryCache(objcache.MemoryConfig{
		MaxSize:       1 << 20,
		MaxObjectSize: 1 << 16,
		TTL:           time.Minute,
	})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	return c
}

// unreachableBackend returns a backend mock with no expectations, so any call
// the read makes to it fails the test.
func unreachableBackend(t *testing.T) backend.ObjectBackend {
	t.Helper()
	return backendtest.NewMockObjectBackend(gomock.NewController(t))
}

// backendHolding returns an in-memory backend storing body at path.
func backendHolding(path, body string) *backendtest.InMemory {
	be := backendtest.NewInMemory()
	be.Put(path, &backendtest.Object{Data: []byte(body), ContentType: cacheType})
	return be
}

// readAll drains and closes a GET result, returning its body.
func readAll(t *testing.T, res *GetResult) string {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

// assertCached fails unless the cache holds want under path.
func assertCached(t *testing.T, c objcache.ObjectCache, path, want string) {
	t.Helper()
	entry, ok := c.Get(path)
	if !ok {
		t.Fatalf("no cache entry under %q", path)
	}
	if string(entry.Data) != want {
		t.Errorf("cache entry under %q = %q, want %q", path, entry.Data, want)
	}
}

// countingLocationsStore answers every location lookup with locs and counts
// the lookups, so a test can assert how many a request made.
func countingLocationsStore(t *testing.T, locs []core.ObjectLocation, lookups *atomic.Int32) *storetest.MockMetadataStore {
	t.Helper()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	objectsStubs(store)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, string) ([]core.ObjectLocation, error) {
			lookups.Add(1)
			return locs, nil
		}).AnyTimes()
	storetest.Permissive(store)
	return store
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestGetObject_CacheHitServesWithoutABackend verifies a hit under the row's
// storage path answers the whole GET, including the row's tag count, without
// touching any backend.
func TestGetObject_CacheHitServesWithoutABackend(t *testing.T) {
	t.Parallel()
	c := newReadCache(t)
	c.PutBytes(cachePathNew, []byte(cachedBody), objcache.EntryMeta{ContentType: cacheType})
	locs := []core.ObjectLocation{{ObjectKey: cacheKey, BackendName: "b1", StorageKey: cachePathNew, TagCount: 2}}
	mgr := newFleet(t, locationsStore(t, locs, nil),
		map[string]backend.ObjectBackend{"b1": unreachableBackend(t)}, &fleetOpts{ObjectCache: c})

	res, err := mgr.GetObject(context.Background(), cacheKey, "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if got := readAll(t, res); got != cachedBody {
		t.Errorf("body = %q, want %q", got, cachedBody)
	}
	if res.TagCount != 2 {
		t.Errorf("TagCount = %d, want 2 from the row", res.TagCount)
	}
	if res.ContentType != cacheType {
		t.Errorf("content type = %q, want %q", res.ContentType, cacheType)
	}
}

// TestGetObject_CacheHitOnAReplicaPath verifies a miss on the first copy's
// path still hits when a later copy's path is cached.
func TestGetObject_CacheHitOnAReplicaPath(t *testing.T) {
	t.Parallel()
	c := newReadCache(t)
	c.PutBytes(cachePathNew, []byte(cachedBody), objcache.EntryMeta{})
	locs := []core.ObjectLocation{
		{ObjectKey: cacheKey, BackendName: "b1", StorageKey: cachePathOld, TagCount: 1},
		{ObjectKey: cacheKey, BackendName: "b2", StorageKey: cachePathNew, TagCount: 1},
	}
	mgr := newFleet(t, locationsStore(t, locs, nil), map[string]backend.ObjectBackend{
		"b1": unreachableBackend(t), "b2": unreachableBackend(t),
	}, &fleetOpts{ObjectCache: c})

	res, err := mgr.GetObject(context.Background(), cacheKey, "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if got := readAll(t, res); got != cachedBody {
		t.Errorf("body = %q, want the replica's cached bytes", got)
	}
}

// TestGetObject_OverwriteNeverServesTheOldPath verifies bytes cached for an
// earlier write are not served once the rows name a new storage path, and
// that the read caches the new bytes under the new path.
func TestGetObject_OverwriteNeverServesTheOldPath(t *testing.T) {
	t.Parallel()
	c := newReadCache(t)
	c.PutBytes(cachePathOld, []byte(cachedBody), objcache.EntryMeta{})
	locs := []core.ObjectLocation{{ObjectKey: cacheKey, BackendName: "b1", StorageKey: cachePathNew, SizeBytes: int64(len(freshBody))}}
	mgr := newFleet(t, locationsStore(t, locs, nil), map[string]backend.ObjectBackend{
		"b1": backendHolding(cachePathNew, freshBody),
	}, &fleetOpts{ObjectCache: c})

	res, err := mgr.GetObject(context.Background(), cacheKey, "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if got := readAll(t, res); got != freshBody {
		t.Fatalf("body = %q, want %q (the old write's bytes were served)", got, freshBody)
	}
	assertCached(t, c, cachePathNew, freshBody)
}

// TestGetObject_MissCachesUnderTheServingCopy verifies a miss that fails over
// to a later copy caches the bytes under that copy's path, not the first
// row's, and reports the serving row's tag count.
func TestGetObject_MissCachesUnderTheServingCopy(t *testing.T) {
	t.Parallel()
	c := newReadCache(t)
	broken := backendtest.NewInMemory()
	broken.SetGetErr(errors.New("backend down"))
	locs := []core.ObjectLocation{
		{ObjectKey: cacheKey, BackendName: "b1", StorageKey: cachePathOld, TagCount: 4},
		{ObjectKey: cacheKey, BackendName: "b2", StorageKey: cachePathNew, TagCount: 4},
	}
	mgr := newFleet(t, locationsStore(t, locs, nil), map[string]backend.ObjectBackend{
		"b1": broken, "b2": backendHolding(cachePathNew, freshBody),
	}, &fleetOpts{ObjectCache: c, Order: []string{"b1", "b2"}})

	res, err := mgr.GetObject(context.Background(), cacheKey, "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if got := readAll(t, res); got != freshBody {
		t.Fatalf("body = %q, want %q", got, freshBody)
	}
	if res.TagCount != 4 {
		t.Errorf("TagCount = %d, want 4", res.TagCount)
	}
	assertCached(t, c, cachePathNew, freshBody)
	if _, ok := c.Get(cachePathOld); ok {
		t.Error("bytes were cached under the copy that failed")
	}
}

// TestGetObject_LookupFailureNeverHits verifies a read whose location lookup
// failed is never answered from the cache, even when the bare key is cached,
// because without rows nothing says which bytes are current.
func TestGetObject_LookupFailureNeverHits(t *testing.T) {
	t.Parallel()
	for _, lookupErr := range []error{core.ErrDBUnavailable, errors.New("query failed")} {
		c := newReadCache(t)
		c.PutBytes(cacheKey, []byte(cachedBody), objcache.EntryMeta{})
		mgr := newFleet(t, locationsStore(t, nil, lookupErr), map[string]backend.ObjectBackend{
			"b1": unreachableBackend(t),
		}, &fleetOpts{ObjectCache: c, DisableDegradedReads: true})

		if res, err := mgr.GetObject(context.Background(), cacheKey, ""); err == nil {
			t.Errorf("lookup error %v: served %q, want the read to fail", lookupErr, readAll(t, res))
		}
	}
}

// TestGetObject_RangedReadSkipsTheCache verifies a ranged GET neither answers
// from a cached entry nor fills one.
func TestGetObject_RangedReadSkipsTheCache(t *testing.T) {
	t.Parallel()
	c := newReadCache(t)
	c.PutBytes(cachePathNew, []byte(cachedBody), objcache.EntryMeta{})
	locs := []core.ObjectLocation{{ObjectKey: cacheKey, BackendName: "b1", StorageKey: cachePathNew, SizeBytes: int64(len(freshBody))}}
	mgr := newFleet(t, locationsStore(t, locs, nil), map[string]backend.ObjectBackend{
		"b1": backendHolding(cachePathNew, freshBody),
	}, &fleetOpts{ObjectCache: c})

	res, err := mgr.GetObject(context.Background(), cacheKey, "bytes=0-1")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if got := readAll(t, res); got != freshBody[:2] {
		t.Errorf("body = %q, want %q from the backend", got, freshBody[:2])
	}
	assertCached(t, c, cachePathNew, cachedBody)
}

// TestGetObject_MissLooksTheKeyUpOnce verifies a GET that misses the cache
// reads from a backend over the same lookup it checked the cache with.
func TestGetObject_MissLooksTheKeyUpOnce(t *testing.T) {
	t.Parallel()
	var lookups atomic.Int32
	locs := []core.ObjectLocation{{ObjectKey: cacheKey, BackendName: "b1", StorageKey: cachePathNew, SizeBytes: int64(len(freshBody))}}
	mgr := newFleet(t, countingLocationsStore(t, locs, &lookups), map[string]backend.ObjectBackend{
		"b1": backendHolding(cachePathNew, freshBody),
	}, &fleetOpts{ObjectCache: newReadCache(t)})

	res, err := mgr.GetObject(context.Background(), cacheKey, "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	readAll(t, res)
	if got := lookups.Load(); got != 1 {
		t.Errorf("location lookups = %d, want 1", got)
	}
}

// TestHeadObject_BackendFallthroughLooksTheKeyUpOnce verifies a HEAD whose
// rows carry no identity reads from a backend over the lookup it already made.
func TestHeadObject_BackendFallthroughLooksTheKeyUpOnce(t *testing.T) {
	t.Parallel()
	var lookups atomic.Int32
	locs := []core.ObjectLocation{{ObjectKey: cacheKey, BackendName: "b1", StorageKey: cachePathNew, SizeBytes: int64(len(freshBody))}}
	mgr := newFleet(t, countingLocationsStore(t, locs, &lookups), map[string]backend.ObjectBackend{
		"b1": backendHolding(cachePathNew, freshBody),
	}, nil)

	if _, err := mgr.HeadObject(context.Background(), cacheKey); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if got := lookups.Load(); got != 1 {
		t.Errorf("location lookups = %d, want 1", got)
	}
}
