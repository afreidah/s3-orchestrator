// -------------------------------------------------------------------------------
// Object Manager - Tag Operations
//
// Author: Alex Freidah
//
// The manager side of object tagging. Tags describe the object rather than any
// one copy of it, so none of these operations touch a backend: the whole set
// lives in the metadata store and every replica shares it.
//
// The store operations already validate the set, take the key lock, and refuse
// a key that holds no copies, so these methods are a thin pass-through that
// exists to keep the transport talking to the manager rather than reaching
// past it into the store.
// -------------------------------------------------------------------------------

package object

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// GetObjectTags returns an object's tag set, ordered by key. An object with no
// tags yields an empty set rather than an error, since an untagged object has
// an empty TagSet rather than a missing one.
//
// The existence check is separate from the read because there is nothing to
// serialise against: a set read while the object is being deleted is a stale
// answer either way, and the caller only needs to know the key held something
// when it asked.
func (o *Manager) GetObjectTags(ctx context.Context, key string) ([]core.Tag, error) {
	exists, err := o.ObjectExists(ctx, key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, core.ErrObjectNotFound
	}
	return o.stores.GetObjectTags(ctx, key)
}

// PutObjectTags replaces an object's whole tag set. An empty set leaves the
// object with no tags, which is what a Tagging document with an empty TagSet
// means and is the same outcome as DeleteObjectTags.
func (o *Manager) PutObjectTags(ctx context.Context, key string, tags []core.Tag) error {
	return o.stores.ReplaceObjectTags(ctx, key, tags)
}

// DeleteObjectTags removes an object's whole tag set. Removing a set that is
// already empty succeeds: the object is still there and still has no tags.
func (o *Manager) DeleteObjectTags(ctx context.Context, key string) error {
	return o.stores.DeleteObjectTags(ctx, key)
}
