// -------------------------------------------------------------------------------
// Ops - Object Tag Tests
//
// Author: Alex Freidah
//
// The tag methods validate the key and translate the store's missing-object
// refusal into the ops sentinel every transport already maps. These cover both,
// plus that other failures pass through untouched so a transport can tell a
// refusal from an outage.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newTagObjects builds an Objects over a mocked API with one bucket
// configured, which is what the key validation checks against.
func newTagObjects(t *testing.T) (*Objects, *opstest.MockObjectAPI) {
	t.Helper()
	api := opstest.NewMockObjectAPI(gomock.NewController(t))
	return NewObjects(ObjectsDeps{
		Objects: api,
		Store:   storetest.NewMockObjectStore(gomock.NewController(t)),
		Config: NewConfigStore(&config.Config{
			Buckets: []config.BucketConfig{{Name: "bucket"}},
		}),
	}), api
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestObjectsTags_ReturnsStoredSet covers the read.
func TestObjectsTags_ReturnsStoredSet(t *testing.T) {
	t.Parallel()
	o, api := newTagObjects(t)
	api.EXPECT().GetObjectTags(gomock.Any(), "bucket/k").
		Return([]core.Tag{{Key: "a", Value: "1"}}, nil)

	got, err := o.Tags(context.Background(), "bucket/k")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(got) != 1 || got[0].Key != "a" {
		t.Errorf("tags = %+v, want the stored set", got)
	}
}

// TestObjectsTags_KeyOutsideAnyBucket verifies a key that names no configured
// bucket is refused before the store is asked.
func TestObjectsTags_KeyOutsideAnyBucket(t *testing.T) {
	t.Parallel()
	o, _ := newTagObjects(t)
	// No GetObjectTags expectation: validation runs first.

	if _, err := o.Tags(context.Background(), "elsewhere/k"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("error = %v, want ErrInvalidKey", err)
	}
}

// TestObjectsTags_MissingObject verifies the store's refusal becomes the ops
// sentinel, which is what every transport maps to 404.
func TestObjectsTags_MissingObject(t *testing.T) {
	t.Parallel()
	o, api := newTagObjects(t)
	api.EXPECT().GetObjectTags(gomock.Any(), gomock.Any()).Return(nil, core.ErrObjectNotFound)

	if _, err := o.Tags(context.Background(), "bucket/gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// TestObjectsTags_OtherFailurePassesThrough verifies an outage is not
// flattened into ErrNotFound, so a transport can tell them apart.
func TestObjectsTags_OtherFailurePassesThrough(t *testing.T) {
	t.Parallel()
	o, api := newTagObjects(t)
	sentinel := errors.New("database unavailable")
	api.EXPECT().GetObjectTags(gomock.Any(), gomock.Any()).Return(nil, sentinel)

	if _, err := o.Tags(context.Background(), "bucket/k"); !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the store error", err)
	}
}

// TestObjectsSetTags_Replaces covers the write and its two refusals.
func TestObjectsSetTags_Replaces(t *testing.T) {
	t.Parallel()
	o, api := newTagObjects(t)
	tags := []core.Tag{{Key: "a", Value: "1"}}
	api.EXPECT().PutObjectTags(gomock.Any(), "bucket/k", tags).Return(nil)

	if err := o.SetTags(context.Background(), "bucket/k", tags); err != nil {
		t.Errorf("SetTags: %v", err)
	}
}

// TestObjectsSetTags_MissingObject verifies the write's refusal maps too.
func TestObjectsSetTags_MissingObject(t *testing.T) {
	t.Parallel()
	o, api := newTagObjects(t)
	api.EXPECT().PutObjectTags(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(core.ErrObjectNotFound)

	if err := o.SetTags(context.Background(), "bucket/gone", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// TestObjectsSetTags_ValidationPassesThrough verifies a tag-shape refusal
// reaches the caller intact, so the transport can render which limit failed.
func TestObjectsSetTags_ValidationPassesThrough(t *testing.T) {
	t.Parallel()
	o, api := newTagObjects(t)
	api.EXPECT().PutObjectTags(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(core.ErrTooManyTags)

	if err := o.SetTags(context.Background(), "bucket/k", nil); !errors.Is(err, core.ErrTooManyTags) {
		t.Errorf("error = %v, want ErrTooManyTags", err)
	}
}

// TestObjectsSetTags_InvalidKey verifies validation runs before the store.
func TestObjectsSetTags_InvalidKey(t *testing.T) {
	t.Parallel()
	o, _ := newTagObjects(t)

	if err := o.SetTags(context.Background(), "", nil); !errors.Is(err, ErrKeyRequired) {
		t.Errorf("error = %v, want ErrKeyRequired", err)
	}
}

// TestObjectsDeleteTags_Clears covers the clear and its refusals.
func TestObjectsDeleteTags_Clears(t *testing.T) {
	t.Parallel()
	o, api := newTagObjects(t)
	api.EXPECT().DeleteObjectTags(gomock.Any(), "bucket/k").Return(nil)

	if err := o.DeleteTags(context.Background(), "bucket/k"); err != nil {
		t.Errorf("DeleteTags: %v", err)
	}
}

// TestObjectsDeleteTags_MissingObject verifies the clear's refusal maps.
func TestObjectsDeleteTags_MissingObject(t *testing.T) {
	t.Parallel()
	o, api := newTagObjects(t)
	api.EXPECT().DeleteObjectTags(gomock.Any(), gomock.Any()).Return(core.ErrObjectNotFound)

	if err := o.DeleteTags(context.Background(), "bucket/gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// TestObjectsDeleteTags_InvalidKey verifies validation runs before the store.
func TestObjectsDeleteTags_InvalidKey(t *testing.T) {
	t.Parallel()
	o, _ := newTagObjects(t)

	if err := o.DeleteTags(context.Background(), "elsewhere/k"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("error = %v, want ErrInvalidKey", err)
	}
}

// TestObjectsDeleteTags_OtherFailurePassesThrough verifies an outage stays
// distinguishable from a missing object.
func TestObjectsDeleteTags_OtherFailurePassesThrough(t *testing.T) {
	t.Parallel()
	o, api := newTagObjects(t)
	sentinel := errors.New("database unavailable")
	api.EXPECT().DeleteObjectTags(gomock.Any(), gomock.Any()).Return(sentinel)

	if err := o.DeleteTags(context.Background(), "bucket/k"); !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the store error", err)
	}
}
