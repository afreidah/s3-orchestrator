// -------------------------------------------------------------------------------
// Ops - Object Operations
//
// Author: Alex Freidah
//
// Reading, writing, listing and removing object data, independent of the
// transport that asked for it. Every byte-moving call goes through the object
// manager so usage accounting fires through the same paths the S3-protocol
// handlers use, and key validation happens here rather than once per transport.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"errors"
	"log/slog"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// MaxUploadSize caps a single upload accepted from an operator interface.
// Transports enforce it while reading the request body so an oversized upload
// is refused before it reaches a backend.
const MaxUploadSize = 512 << 20

// deletePrefixPageSize is how many keys one listing page collects while
// walking a prefix ahead of a bulk delete.
const deletePrefixPageSize = 1000

// listPageSize is how many keys one browse page holds.
const listPageSize = 1000

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// ObjectsDeps holds the collaborators Objects requires.
type ObjectsDeps struct {
	Objects ObjectAPI
	Store   ObjectStore
	Buckets BucketMatcher
}

// Objects serves the object read and write operations shared by the admin API
// and the web UI.
type Objects struct {
	log     *slog.Logger
	objects ObjectAPI
	store   ObjectStore
	buckets BucketMatcher
}

// NewObjects is the explicit-deps constructor.
func NewObjects(d ObjectsDeps) *Objects {
	must.NotNil("d.Objects", d.Objects)
	must.NotNil("d.Store", d.Store)
	must.NotNil("d.Buckets", d.Buckets)
	return &Objects{
		log:     slog.Default().With(logfmt.Component("ops")),
		objects: d.Objects,
		store:   d.Store,
		buckets: d.Buckets,
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// List returns one page of the namespace under prefix. A delimiter groups the
// keys below it into common prefixes; an empty delimiter lists every key flat,
// which is what a caller counting or sweeping a subtree wants. A non-empty
// continuation resumes a previously truncated page.
func (o *Objects) List(ctx context.Context, prefix, delimiter, continuation string) (*core.ListObjectsResult, error) {
	return o.store.ListObjects(ctx, prefix, delimiter, continuation, listPageSize)
}

// Locations reports every backend holding a copy of one key, for callers
// answering where an object actually lives.
func (o *Objects) Locations(ctx context.Context, key string) ([]core.ObjectLocation, error) {
	if key == "" {
		return nil, ErrKeyRequired
	}
	return o.store.GetAllObjectLocations(ctx, key)
}

// Tags reports one object's tag set, ordered by key. An object carrying none
// yields an empty set; a key holding no copies reports ErrNotFound.
func (o *Objects) Tags(ctx context.Context, key string) ([]core.Tag, error) {
	if err := o.validateKey(key); err != nil {
		return nil, err
	}
	tags, err := o.objects.GetObjectTags(ctx, key)
	if err != nil {
		return nil, notFound(err)
	}
	return tags, nil
}

// SetTags replaces one object's whole tag set. An empty set leaves the object
// untagged, which is the same outcome as DeleteTags. A tag set of the wrong
// shape is rejected as invalid, naming the limit it broke.
func (o *Objects) SetTags(ctx context.Context, key string, tags []core.Tag) error {
	if err := o.validateKey(key); err != nil {
		return err
	}
	err := o.objects.PutObjectTags(ctx, key, tags)
	if core.IsTagValidationError(err) {
		return &Rejection{Kind: RejectInvalid, err: err}
	}
	return notFound(err)
}

// DeleteTags removes one object's whole tag set.
func (o *Objects) DeleteTags(ctx context.Context, key string) error {
	if err := o.validateKey(key); err != nil {
		return err
	}
	return notFound(o.objects.DeleteObjectTags(ctx, key))
}

// notFound reports a key holding no copies as ErrNotFound and passes any other
// error, nil included, through.
func notFound(err error) error {
	if errors.Is(err, core.ErrObjectNotFound) {
		return ErrNotFound
	}
	return err
}

// Get streams one object. The caller closes the returned body. Reports
// ErrNotFound when no copy of the key is recorded.
func (o *Objects) Get(ctx context.Context, key string) (*s3be.GetObjectResult, error) {
	if err := o.validateKey(key); err != nil {
		return nil, err
	}

	result, err := o.objects.GetObject(ctx, key, "")
	if err != nil {
		return nil, notFound(err)
	}
	// The operations layer moves bytes; the object-level metadata riding on the
	// manager's result is for the S3 surface to render, not for this caller.
	return result.GetObjectResult, nil
}

// Put stores one object and returns its ETag. Size is the exact byte count of
// the body; an empty content type leaves the choice to the backend.
func (o *Objects) Put(ctx context.Context, req *object.PutObjectRequest) (string, error) {
	if err := o.validateKey(req.Key); err != nil {
		return "", err
	}

	etag, err := o.objects.PutObject(ctx, req)
	if err != nil {
		return "", err
	}

	o.log.InfoContext(ctx, "stored object", "key", req.Key, "size", req.Size)
	return etag, nil
}

// Delete removes one object and every copy of it.
func (o *Objects) Delete(ctx context.Context, key string) error {
	if err := o.validateKey(key); err != nil {
		return err
	}

	if err := o.objects.DeleteObject(ctx, key); err != nil {
		return err
	}

	o.log.InfoContext(ctx, "deleted object", "key", key)
	return nil
}

// DeletePrefix removes every object under prefix, one listing page at a time
// so the key set never has to be held whole. The result counts the keys
// removed as succeeded, so a caller can tell a no-op from a mass removal, and
// the ones that could not be removed as failed.
func (o *Objects) DeletePrefix(ctx context.Context, prefix string) (batch.Summary, error) {
	if prefix == "" {
		return batch.Summary{}, ErrPrefixRequired
	}

	var res batch.Summary
	pager := batch.Pager[string, string]{
		PageSize: batch.FixedPage(deletePrefixPageSize),
		List: func(ctx context.Context, limit int, after string) ([]string, error) {
			page, err := o.objects.ListObjects(ctx, prefix, "", after, limit)
			if err != nil {
				return nil, err
			}
			return core.KeysOf(page.Objects), nil
		},
		CursorOf: func(key string) string { return key },
	}
	if _, err := pager.Walk(ctx, func(ctx context.Context, keys []string) (batch.Step, error) {
		o.deletePage(ctx, keys, &res)
		return batch.Step{}, nil
	}); err != nil {
		return res, err
	}

	o.log.InfoContext(ctx, "prefix delete completed", "prefix", prefix, "deleted", res.Succeeded, "failed", res.Failed)
	return res, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// deletePage removes one page of keys and folds the per-key outcomes into res.
func (o *Objects) deletePage(ctx context.Context, keys []string, res *batch.Summary) {
	res.Planned += len(keys)
	res.Attempted += len(keys)
	for _, item := range o.objects.DeleteObjects(ctx, keys) {
		if item.Err != nil {
			res.Failed++
		} else {
			res.Succeeded++
		}
	}
}

// validateKey rejects a key that is empty or outside every declared virtual
// bucket, before any backend is contacted. Buckets from both config and the
// store count, matching what the S3 path accepts.
func (o *Objects) validateKey(key string) error {
	if key == "" {
		return ErrKeyRequired
	}
	if !o.buckets.HasPrefix(key) {
		return ErrInvalidKey
	}
	return nil
}
