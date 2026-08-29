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
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// newTagTestManager builds a Manager over a mocked store, which is all the tag
// methods touch.
func newTagTestManager(t *testing.T) (*Manager, *storetest.MockMetadataStore) {
	t.Helper()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	return newListTestManager(store), store
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
