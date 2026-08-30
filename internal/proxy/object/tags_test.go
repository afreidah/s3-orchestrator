// -------------------------------------------------------------------------------
// Object Manager - Tag Operation Tests
//
// Author: Alex Freidah
//
// The manager's tag methods are a pass-through to the store, so what these
// cover is the one thing the manager adds: the read refuses a key that holds
// no copies, rather than answering with an empty set that a caller would read
// as "this object has no tags".
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// newTagTestManager builds a Manager over a mocked store, which is all the tag
// methods touch. The location cache is real because the tag writes invalidate
// it, and a Manager in production never holds a nil one.
func newTagTestManager(t *testing.T) (*Manager, *storetest.MockMetadataStore) {
	t.Helper()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	m := newListTestManager(store)
	m.cache = NewLocationCache(time.Minute)
	t.Cleanup(m.cache.Close)
	return m, store
}

// newTagCacheTestManager adds a real object data cache to the tag fixture, so
// the invalidation a tag write owes the cache can be observed rather than
// asserted against a mock's expectations.
func newTagCacheTestManager(t *testing.T) (*Manager, *storetest.MockMetadataStore, objcache.ObjectCache) {
	t.Helper()
	m, store := newTagTestManager(t)
	c, err := objcache.NewMemoryCache(objcache.MemoryConfig{
		MaxSize:       1 << 20,
		MaxObjectSize: 1 << 16,
		TTL:           time.Minute,
	})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	m.objectCache = c
	return m, store, c
}

// TestGetObjectTags_ReturnsStoredSet verifies the set comes back as the store
// handed it over, with no reordering or filtering in the manager.
func TestGetObjectTags_ReturnsStoredSet(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)
	want := []core.Tag{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}}

	store.EXPECT().GetAllObjectLocations(gomock.Any(), "k").
		Return([]core.ObjectLocation{{ObjectKey: "k", BackendName: "b1"}}, nil)
	store.EXPECT().GetObjectTags(gomock.Any(), "k").Return(want, nil)

	got, err := m.GetObjectTags(context.Background(), "k")
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if len(got) != 2 || got[0].Key != "a" || got[1].Key != "b" {
		t.Errorf("tags = %+v, want %+v", got, want)
	}
}

// TestGetObjectTags_UntaggedObject verifies an object holding no tags reads as
// an empty set rather than an error, so the endpoint can answer 200 with an
// empty TagSet.
func TestGetObjectTags_UntaggedObject(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)

	store.EXPECT().GetAllObjectLocations(gomock.Any(), "k").
		Return([]core.ObjectLocation{{ObjectKey: "k", BackendName: "b1"}}, nil)
	store.EXPECT().GetObjectTags(gomock.Any(), "k").Return([]core.Tag{}, nil)

	got, err := m.GetObjectTags(context.Background(), "k")
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected an empty set, got %+v", got)
	}
}

// TestGetObjectTags_MissingObject verifies a key holding no copies is refused
// rather than read. Without the check the caller cannot tell an untagged
// object from one that is not there.
func TestGetObjectTags_MissingObject(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)

	store.EXPECT().GetAllObjectLocations(gomock.Any(), "gone").
		Return([]core.ObjectLocation{}, nil)
	// No GetObjectTags expectation: the read must not happen.

	if _, err := m.GetObjectTags(context.Background(), "gone"); !errors.Is(err, core.ErrObjectNotFound) {
		t.Errorf("error = %v, want ErrObjectNotFound", err)
	}
}

// TestGetObjectTags_ExistenceCheckError verifies a failure to establish whether
// the object exists surfaces rather than being read as absent.
func TestGetObjectTags_ExistenceCheckError(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)
	sentinel := errors.New("store unavailable")

	store.EXPECT().GetAllObjectLocations(gomock.Any(), "k").Return(nil, sentinel)

	if _, err := m.GetObjectTags(context.Background(), "k"); !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the store error", err)
	}
}

// TestGetObjectTags_ReadError verifies a failure reading the set surfaces
// rather than being reported as an object with no tags.
func TestGetObjectTags_ReadError(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)
	sentinel := errors.New("read failed")

	store.EXPECT().GetAllObjectLocations(gomock.Any(), "k").
		Return([]core.ObjectLocation{{ObjectKey: "k", BackendName: "b1"}}, nil)
	store.EXPECT().GetObjectTags(gomock.Any(), "k").Return(nil, sentinel)

	if _, err := m.GetObjectTags(context.Background(), "k"); !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the store error", err)
	}
}

// TestPutObject_CarriesTagsIntoTheRecord verifies a write's tag set reaches
// the commit, so the object and its tags land in one transaction rather than
// leaving the object briefly untagged.
func TestPutObject_CarriesTagsIntoTheRecord(t *testing.T) {
	store, calls := putObjectStore(t, "b1")
	f := newFleet(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
	}, nil)

	tags := []core.Tag{{Key: "retain", Value: "30d"}}
	if _, err := f.PutObject(context.Background(), &PutObjectRequest{
		Key: "tagged", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain", Tags: tags,
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.recordObject) != 1 {
		t.Fatalf("expected one record, got %d", len(calls.recordObject))
	}
	got := calls.recordObject[0].Tags
	if len(got) != 1 || got[0].Key != "retain" || got[0].Value != "30d" {
		t.Errorf("recorded tags = %+v, want the set the write carried", got)
	}
}

// TestPutObject_UntaggedWriteRecordsNoTags verifies a write with no tags
// commits an empty set, which is what clears whatever the key held: a PUT is
// a full replacement rather than a merge.
func TestPutObject_UntaggedWriteRecordsNoTags(t *testing.T) {
	store, calls := putObjectStore(t, "b1")
	f := newFleet(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
	}, nil)

	if _, err := f.PutObject(context.Background(), &PutObjectRequest{
		Key: "plain", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.recordObject) != 1 {
		t.Fatalf("expected one record, got %d", len(calls.recordObject))
	}
	if len(calls.recordObject[0].Tags) != 0 {
		t.Errorf("expected no tags recorded, got %+v", calls.recordObject[0].Tags)
	}
}

// TestResolveCopyTags_CopyDirectiveReadsTheSource verifies the default
// directive gives the destination whatever the source carries, which means
// reading the source's set rather than taking one from the request.
func TestResolveCopyTags_CopyDirectiveReadsTheSource(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)
	want := []core.Tag{{Key: "inherited", Value: "yes"}}

	store.EXPECT().GetObjectTags(gomock.Any(), "src").Return(want, nil)

	got, err := m.resolveCopyTags(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"})
	if err != nil {
		t.Fatalf("resolveCopyTags: %v", err)
	}
	if len(got) != 1 || got[0].Key != "inherited" {
		t.Errorf("tags = %+v, want the source's set", got)
	}
}

// TestResolveCopyTags_ReplaceIgnoresTheSource verifies REPLACE takes the
// request's set and never reads the source, so a copy can be given different
// tags without inheriting any.
func TestResolveCopyTags_ReplaceIgnoresTheSource(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)
	// No GetObjectTags expectation: reading the source would be wrong here.
	_ = store

	supplied := []core.Tag{{Key: "fresh", Value: "1"}}
	got, err := m.resolveCopyTags(context.Background(), &CopyObjectRequest{
		SourceKey: "src", DestKey: "dst", ReplaceTags: true, Tags: supplied,
	})
	if err != nil {
		t.Fatalf("resolveCopyTags: %v", err)
	}
	if len(got) != 1 || got[0].Key != "fresh" {
		t.Errorf("tags = %+v, want the request's set", got)
	}
}

// TestResolveCopyTags_ReplaceWithNoTagsStrips verifies a REPLACE carrying no
// tags leaves the destination untagged, which is how a client drops a copy's
// tags rather than inheriting them.
func TestResolveCopyTags_ReplaceWithNoTagsStrips(t *testing.T) {
	t.Parallel()
	m, _ := newTagTestManager(t)

	got, err := m.resolveCopyTags(context.Background(), &CopyObjectRequest{
		SourceKey: "src", DestKey: "dst", ReplaceTags: true,
	})
	if err != nil {
		t.Fatalf("resolveCopyTags: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no tags, got %+v", got)
	}
}

// TestResolveCopyTags_SourceReadError verifies a failure reading the source's
// set aborts the copy rather than silently producing an untagged destination.
func TestResolveCopyTags_SourceReadError(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)
	sentinel := errors.New("read failed")

	store.EXPECT().GetObjectTags(gomock.Any(), "src").Return(nil, sentinel)

	if _, err := m.resolveCopyTags(context.Background(), &CopyObjectRequest{
		SourceKey: "src", DestKey: "dst",
	}); !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the store error", err)
	}
}

// TestPutObjectTags_PassesThrough verifies the set reaches the store unaltered.
// Validation and the existence check live there, so the manager adds nothing.
func TestPutObjectTags_PassesThrough(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)
	tags := []core.Tag{{Key: "a", Value: "1"}}

	store.EXPECT().ReplaceObjectTags(gomock.Any(), "k", tags).Return(nil)

	if err := m.PutObjectTags(context.Background(), "k", tags); err != nil {
		t.Errorf("PutObjectTags: %v", err)
	}
}

// TestPutObjectTags_SurfacesStoreError verifies a store refusal reaches the
// caller intact, which is what lets the transport map it to an S3 code.
func TestPutObjectTags_SurfacesStoreError(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)

	store.EXPECT().ReplaceObjectTags(gomock.Any(), "k", gomock.Any()).
		Return(core.ErrTooManyTags)

	err := m.PutObjectTags(context.Background(), "k", []core.Tag{{Key: "a", Value: "1"}})
	if !errors.Is(err, core.ErrTooManyTags) {
		t.Errorf("error = %v, want ErrTooManyTags", err)
	}
}

// TestDeleteObjectTags_PassesThrough verifies the delete reaches the store.
func TestDeleteObjectTags_PassesThrough(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)

	store.EXPECT().DeleteObjectTags(gomock.Any(), "k").Return(nil)

	if err := m.DeleteObjectTags(context.Background(), "k"); err != nil {
		t.Errorf("DeleteObjectTags: %v", err)
	}
}

// TestDeleteObjectTags_SurfacesStoreError verifies a refusal for a key that
// holds nothing reaches the caller as ErrObjectNotFound.
func TestDeleteObjectTags_SurfacesStoreError(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)

	store.EXPECT().DeleteObjectTags(gomock.Any(), "gone").Return(core.ErrObjectNotFound)

	if err := m.DeleteObjectTags(context.Background(), "gone"); !errors.Is(err, core.ErrObjectNotFound) {
		t.Errorf("error = %v, want ErrObjectNotFound", err)
	}
}

// taggedLocationsStore wires one location and a tag count for it. The count is
// declared ahead of the permissive defaults, which answer every method with a
// zero value, so this is the expectation gomock matches.
func taggedLocationsStore(t *testing.T, tagCount int) *storetest.MockMetadataStore {
	t.Helper()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	objectsStubs(store)
	store.EXPECT().CountObjectTags(gomock.Any(), "key").Return(tagCount, nil).AnyTimes()
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{ObjectKey: "key", BackendName: "b1"}}, nil).AnyTimes()
	storetest.Permissive(store)
	return store
}

// TestGetObject_CarriesTheTagCount verifies a read carries the count out on the
// result the transport renders its header from. The pieces are covered on their
// own above; what this pins is the wiring between them, which is where a count
// that never leaves the store would hide.
func TestGetObject_CarriesTheTagCount(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	if _, err := be.PutObject(context.Background(), "key", bytes.NewReader([]byte("hello")), 5, "text/plain", nil); err != nil {
		t.Fatalf("seed PutObject: %v", err)
	}
	mgr := newFleet(t, taggedLocationsStore(t, 3), map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	if result.TagCount != 3 {
		t.Errorf("TagCount = %d, want 3", result.TagCount)
	}
}

// TestHeadObject_CarriesTheTagCount verifies HEAD reports the count too. HEAD
// never consults the object data cache, so unlike GET it counts on every
// request and has no cached value to fall back on.
func TestHeadObject_CarriesTheTagCount(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	if _, err := be.PutObject(context.Background(), "key", bytes.NewReader([]byte("hello")), 5, "text/plain", nil); err != nil {
		t.Fatalf("seed PutObject: %v", err)
	}
	mgr := newFleet(t, taggedLocationsStore(t, 2), map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if result.TagCount != 2 {
		t.Errorf("TagCount = %d, want 2", result.TagCount)
	}
}

// TestCountObjectTags_ReportsTheStoreCount verifies the read path reports what
// the store holds, since that number is what decides whether the response
// carries a tagging-count header at all.
func TestCountObjectTags_ReportsTheStoreCount(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)

	store.EXPECT().CountObjectTags(gomock.Any(), "k").Return(3, nil)

	if n := m.countObjectTags(context.Background(), "k"); n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
}

// TestCountObjectTags_StoreFailureReportsNone verifies an unreadable count is
// reported as none rather than surfacing. The object's bytes are already
// correct by the time this runs, so a GET must not fail because one advisory
// header could not be filled in.
func TestCountObjectTags_StoreFailureReportsNone(t *testing.T) {
	t.Parallel()
	m, store := newTagTestManager(t)

	store.EXPECT().CountObjectTags(gomock.Any(), "k").
		Return(0, errors.New("store unavailable"))

	if n := m.countObjectTags(context.Background(), "k"); n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// TestPutObjectTags_DropsTheCachedObject verifies a tag write invalidates the
// cached copy. The entry carries the tag count that answers a cache-hit GET
// without consulting the store, so an entry left in place would keep reporting
// the count the object had before the write.
func TestPutObjectTags_DropsTheCachedObject(t *testing.T) {
	t.Parallel()
	m, store, c := newTagCacheTestManager(t)
	c.PutBytes("k", []byte("data"), objcache.EntryMeta{TagCount: 1})

	store.EXPECT().ReplaceObjectTags(gomock.Any(), "k", gomock.Any()).Return(nil)

	if err := m.PutObjectTags(context.Background(), "k", []core.Tag{
		{Key: "a", Value: "1"}, {Key: "b", Value: "2"},
	}); err != nil {
		t.Fatalf("PutObjectTags: %v", err)
	}
	if _, ok := c.Get("k"); ok {
		t.Error("cached entry survived a tag write, so a hit would serve a stale count")
	}
}

// TestPutObjectTags_FailedWriteKeepsTheCachedObject verifies a refused write
// leaves the cache alone. Nothing changed, so dropping the entry would cost a
// re-read of the object for no reason.
func TestPutObjectTags_FailedWriteKeepsTheCachedObject(t *testing.T) {
	t.Parallel()
	m, store, c := newTagCacheTestManager(t)
	c.PutBytes("k", []byte("data"), objcache.EntryMeta{TagCount: 1})

	store.EXPECT().ReplaceObjectTags(gomock.Any(), "k", gomock.Any()).
		Return(core.ErrTooManyTags)

	if err := m.PutObjectTags(context.Background(), "k", []core.Tag{{Key: "a", Value: "1"}}); err == nil {
		t.Fatal("expected the store refusal to surface")
	}
	if _, ok := c.Get("k"); !ok {
		t.Error("a refused tag write dropped the cached object")
	}
}

// TestDeleteObjectTags_DropsTheCachedObject verifies clearing a set invalidates
// the cached copy, which is the direction that matters most: an entry left in
// place would keep advertising tags the object no longer has.
func TestDeleteObjectTags_DropsTheCachedObject(t *testing.T) {
	t.Parallel()
	m, store, c := newTagCacheTestManager(t)
	c.PutBytes("k", []byte("data"), objcache.EntryMeta{TagCount: 2})

	store.EXPECT().DeleteObjectTags(gomock.Any(), "k").Return(nil)

	if err := m.DeleteObjectTags(context.Background(), "k"); err != nil {
		t.Fatalf("DeleteObjectTags: %v", err)
	}
	if _, ok := c.Get("k"); ok {
		t.Error("cached entry survived a tag delete, so a hit would still report tags")
	}
}

// TestTryGetObjectCache_ServesTheStoredTagCount verifies a cache hit answers
// with the count its entry was filled with. A hit never reaches the store, so
// this is the only thing keeping the header on the responses the cache serves.
func TestTryGetObjectCache_ServesTheStoredTagCount(t *testing.T) {
	t.Parallel()
	m, _, c := newTagCacheTestManager(t)
	c.PutBytes("k", []byte("data"), objcache.EntryMeta{ContentType: "text/plain", TagCount: 2})

	got, ok := m.tryGetObjectCache(context.Background(), "k", "")
	if !ok {
		t.Fatal("expected a cache hit")
	}
	if got.TagCount != 2 {
		t.Errorf("TagCount = %d, want 2", got.TagCount)
	}
}

// TestPopulateObjectCache_StoresTheTagCount verifies the count reaches the
// entry a read populates, so a later hit has something to serve. The tee hands
// the buffer over only once the body reads clean, which is why the assertion
// follows a full drain.
func TestPopulateObjectCache_StoresTheTagCount(t *testing.T) {
	t.Parallel()
	m, _, c := newTagCacheTestManager(t)
	res := &backend.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader([]byte("data"))),
		Size: 4,
	}

	if err := m.populateObjectCache("k", "", res, 5); err != nil {
		t.Fatalf("populateObjectCache: %v", err)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatalf("drain body: %v", err)
	}

	entry, ok := c.Get("k")
	if !ok {
		t.Fatal("expected the read to populate the cache")
	}
	if entry.TagCount != 5 {
		t.Errorf("cached TagCount = %d, want 5", entry.TagCount)
	}
}
