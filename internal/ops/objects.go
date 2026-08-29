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
	"io"
	"log/slog"
	"strings"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// MaxUploadSize caps a single upload accepted from an operator interface.
// Transports enforce it while reading the request body so an oversized upload
// is refused before it reaches a backend.
const MaxUploadSize = 512 << 20

// deletePrefixPageSize is how many keys one listing page collects while
// walking a prefix ahead of a bulk delete.
const deletePrefixPageSize = 1000

// defaultListMaxKeys caps one browse page when the caller asks for no limit.
const defaultListMaxKeys = 1000

// DeletePrefixResult reports what a prefix delete removed. Failed is non-zero
// when some copies could not be removed, which leaves the prefix partially
// deleted rather than untouched.
type DeletePrefixResult struct {
	Deleted int
	Failed  int
	Total   int
}

// ObjectsDeps holds the collaborators Objects requires.
type ObjectsDeps struct {
	Objects ObjectAPI
	Store   ObjectStore
	Config  *ConfigStore
}

// Objects serves the object read and write operations shared by the admin API
// and the web UI.
type Objects struct {
	log     *slog.Logger
	objects ObjectAPI
	store   ObjectStore
	cfg     *ConfigStore
}

// NewObjects is the explicit-deps constructor.
func NewObjects(d ObjectsDeps) *Objects {
	must.NotNil("d.Objects", d.Objects)
	must.NotNil("d.Store", d.Store)
	must.NotNil("d.Config", d.Config)
	return &Objects{
		log:     slog.Default().With(logfmt.Component("ops")),
		objects: d.Objects,
		store:   d.Store,
		cfg:     d.Config,
	}
}

// List returns one page of the namespace under prefix. A delimiter groups the
// keys below it into common prefixes; an empty delimiter lists every key flat,
// which is what a caller counting or sweeping a subtree wants. A non-empty
// continuation resumes a previously truncated page, and maxKeys <= 0 falls
// back to the default page size.
func (o *Objects) List(ctx context.Context, prefix, delimiter, continuation string, maxKeys int) (*core.ListDelimitedResult, error) {
	if maxKeys <= 0 {
		maxKeys = defaultListMaxKeys
	}
	if delimiter != "" {
		return o.store.ListObjectsDelimited(ctx, prefix, delimiter, continuation, maxKeys)
	}

	// A flat listing is a different store call, mapped onto the same page
	// shape so a caller reads one result type either way.
	flat, err := o.store.ListObjects(ctx, prefix, continuation, maxKeys)
	if err != nil {
		return nil, err
	}
	return &core.ListDelimitedResult{
		Objects:               flat.Objects,
		IsTruncated:           flat.IsTruncated,
		NextContinuationToken: flat.NextContinuationToken,
	}, nil
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
		if errors.Is(err, core.ErrObjectNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return tags, nil
}

// SetTags replaces one object's whole tag set. An empty set leaves the object
// untagged, which is the same outcome as DeleteTags.
func (o *Objects) SetTags(ctx context.Context, key string, tags []core.Tag) error {
	if err := o.validateKey(key); err != nil {
		return err
	}
	if err := o.objects.PutObjectTags(ctx, key, tags); err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// DeleteTags removes one object's whole tag set.
func (o *Objects) DeleteTags(ctx context.Context, key string) error {
	if err := o.validateKey(key); err != nil {
		return err
	}
	if err := o.objects.DeleteObjectTags(ctx, key); err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// Get streams one object. The caller closes the returned body. Reports
// ErrNotFound when no copy of the key is recorded.
func (o *Objects) Get(ctx context.Context, key string) (*s3be.GetObjectResult, error) {
	if err := o.validateKey(key); err != nil {
		return nil, err
	}

	result, err := o.objects.GetObject(ctx, key, "")
	if err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return result, nil
}

// Put stores one object and returns its ETag. size is the exact byte count of
// body; contentType may be empty, in which case the backend decides.
func (o *Objects) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) (string, error) {
	if err := o.validateKey(key); err != nil {
		return "", err
	}

	etag, err := o.objects.PutObject(ctx, &object.PutObjectRequest{
		Key: key, Body: body, Size: size, ContentType: contentType,
	})
	if err != nil {
		return "", err
	}

	o.log.InfoContext(ctx, "stored object", "key", key, "size", size)
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
// so the key set never has to be held whole. observer, when non-nil, receives
// an end step per key as each page completes. The result reports how many keys
// were removed, so a caller can tell a no-op from a mass removal.
func (o *Objects) DeletePrefix(ctx context.Context, prefix string, observer progress.Observer) (DeletePrefixResult, error) {
	if prefix == "" {
		return DeletePrefixResult{}, ErrPrefixRequired
	}

	var res DeletePrefixResult
	startAfter := ""
	for {
		page, err := o.objects.ListObjects(ctx, prefix, "", startAfter, deletePrefixPageSize)
		if err != nil {
			return res, err
		}

		keys := make([]string, 0, len(page.Objects))
		for i := range page.Objects {
			keys = append(keys, page.Objects[i].ObjectKey)
		}
		o.deletePage(ctx, keys, observer, &res)

		if !page.IsTruncated {
			break
		}
		startAfter = page.NextContinuationToken
	}

	o.log.InfoContext(ctx, "prefix delete completed", "prefix", prefix, "deleted", res.Deleted, "failed", res.Failed)
	return res, nil
}

// deletePage removes one page of keys and folds the per-key outcomes into res,
// reporting each one through the observer as the batch reports it.
func (o *Objects) deletePage(ctx context.Context, keys []string, observer progress.Observer, res *DeletePrefixResult) {
	if len(keys) == 0 {
		return
	}

	res.Total += len(keys)
	for _, item := range o.objects.DeleteObjects(ctx, keys) {
		status := "ok"
		if item.Err != nil {
			status = "failed"
			res.Failed++
		} else {
			res.Deleted++
		}
		if observer != nil {
			observer(progress.Step{Label: item.Key, Phase: progress.PhaseEnd, Status: status})
		}
	}
}

// validateKey rejects a key that is empty or outside every configured virtual
// bucket, before any backend is contacted.
func (o *Objects) validateKey(key string) error {
	if key == "" {
		return ErrKeyRequired
	}
	cfg := o.cfg.Load()
	if cfg == nil {
		return ErrInvalidKey
	}
	for i := range cfg.Buckets {
		if strings.HasPrefix(key, cfg.Buckets[i].Name+"/") {
			return nil
		}
	}
	return ErrInvalidKey
}
