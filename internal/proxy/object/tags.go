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

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// GetObjectTags returns an object's tag set, ordered by key. An untagged object
// yields an empty set, and a missing one ErrObjectNotFound. The existence check
// and the read are not atomic; a read racing a delete is stale either way.
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
	if err := o.stores.ReplaceObjectTags(ctx, key, tags); err != nil {
		return err
	}
	o.invalidateObjectCaches(key)
	return nil
}

// DeleteObjectTags removes an object's whole tag set. Removing a set that is
// already empty succeeds: the object is still there and still has no tags.
func (o *Manager) DeleteObjectTags(ctx context.Context, key string) error {
	if err := o.stores.DeleteObjectTags(ctx, key); err != nil {
		return err
	}
	o.invalidateObjectCaches(key)
	return nil
}

// countObjectTags reports how many tags a key carries, for the tagging-count
// header the read path emits. A store failure, including during a degraded
// read, reports zero so the header is left off rather than failing the read.
func (o *Manager) countObjectTags(ctx context.Context, key string) int {
	n, err := o.stores.CountObjectTags(ctx, key)
	if err != nil {
		o.log.WarnContext(ctx, "failed to count object tags, omitting tagging count",
			"key", key, logfmt.Err(err))
		return 0
	}
	return n
}
