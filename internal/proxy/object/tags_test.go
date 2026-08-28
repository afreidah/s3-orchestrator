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
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

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
