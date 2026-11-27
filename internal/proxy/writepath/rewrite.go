// -------------------------------------------------------------------------------
// Write Coordinator - Rewriting an Object's Copies
//
// Author: Alex Freidah
//
// A rewrite pass takes copies from one stored form to another. The copies of
// one object that hold the same bytes are rewritten from one read: a copy is
// read, its bytes are reformed once into a buffer, the buffer is checked to
// read back as the bytes the content hash covers, and it lands on every copy's
// own backend in place of that copy. Each copy is claimed, written and placed
// on its own, so one that fails or was replaced meanwhile leaves the others
// recorded.
//
// A source that cannot be read, or whose bytes do not match the hash, gives
// way to the next copy, the rule backend.CopyFromAny follows too. A refusal
// any source would repeat, such as an encoding that misses the ratio, ends the
// attempt for every copy.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"errors"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// RewriteRequest takes the copies of one object to the stored form Want names,
// reforming their bytes through Layers. Every copy must hold the bytes the
// client wrote, which the caller establishes from their content hash, since
// each is replaced by the reform of whichever copy is read.
type RewriteRequest struct {
	Copies []core.ObjectLocation
	Layers *storedform.Layers
	Want   core.StoredFlags
}

// rewriteSource is one read's bytes in the requested form, with the content
// type and metadata the source stored them under.
type rewriteSource struct {
	*storedform.Reformed
	contentType string
	metadata    map[string]string
}

// integrityOpRewrite labels the integrity check a rewrite makes of its reform.
const integrityOpRewrite = "rewrite"

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// RewriteCopies rewrites each of the request's copies in the form it wants and
// returns one error per copy, in order, nil for a copy rewritten. A copy's
// error is what landing it returned, core.ErrCopyChanged when a newer write
// replaced it and core.ErrTargetRefused when its backend had no room, or, when
// no copy could be read and reformed, the error of the last one tried.
func (w *Coordinator) RewriteCopies(ctx context.Context, req *RewriteRequest) []error {
	errs := make([]error, len(req.Copies))
	src, err := w.reformAny(ctx, req)
	if err != nil {
		for i := range errs {
			errs[i] = err
		}
		return errs
	}
	defer src.Release()
	for i := range req.Copies {
		errs[i] = w.landRewrite(ctx, &req.Copies[i], src)
	}
	return errs
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// reformAny reads and reforms the request's copies in order until one gives
// bytes that verify. A failure any other source would repeat ends the walk.
func (w *Coordinator) reformAny(ctx context.Context, req *RewriteRequest) (*rewriteSource, error) {
	var lastErr error
	for i := range req.Copies {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		loc := &req.Copies[i]
		src, err := w.reformFrom(ctx, req, loc)
		if err == nil {
			return src, nil
		}
		if !sourceBound(err) {
			return nil, err
		}
		lastErr = err
		if i < len(req.Copies)-1 {
			w.log.WarnContext(ctx, "rewrite source failed, trying the next copy",
				"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
		}
	}
	return nil, lastErr
}

// reformFrom reads loc and reforms its bytes into the requested form, checking
// the result reads back as the bytes loc's content hash covers.
func (w *Coordinator) reformFrom(ctx context.Context, req *RewriteRequest, loc *core.ObjectLocation) (*rewriteSource, error) {
	be, err := w.core.GetBackend(loc.BackendName)
	if err != nil {
		return nil, err
	}
	src := backend.Endpoint{Name: loc.BackendName, Backend: be}
	read, cancel, err := w.core.ReadCopy(ctx, src, core.StoragePath(loc.ObjectKey, loc.StorageKey), loc.SizeBytes)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer func() { _ = read.Body.Close() }()
	out, err := req.Layers.Reform(ctx, read.Body, read.Size, &loc.StoredForm, req.Want, nil)
	if err != nil {
		return nil, err
	}
	if err := req.Layers.Verify(ctx, w.log, integrityOpRewrite, loc, out); err != nil {
		out.Release()
		return nil, err
	}
	return &rewriteSource{Reformed: out, contentType: read.ContentType, metadata: read.Metadata}, nil
}

// landRewrite replaces dst with the reformed bytes: a copy on dst's own
// backend at a path of its own, claimed for exactly the bytes it holds and
// placed in dst's stead.
func (w *Coordinator) landRewrite(ctx context.Context, dst *core.ObjectLocation, src *rewriteSource) error {
	be, err := w.core.GetBackend(dst.BackendName)
	if err != nil {
		return err
	}
	intent := NewCopyIntent(dst.ObjectKey, dst.BackendName, src.Size, src.Form)
	placement := &core.Placement{Source: dst, Intent: intent, RemoveSource: true, Reformed: true, Reasons: rewriteMove.Reasons}
	_, err = w.landCopy(ctx, placement, rewriteMove.Accepts, func(ctx context.Context) error {
		_, err := w.core.WriteCopy(ctx, &backend.PutRequest{
			Dst:         backend.Endpoint{Name: dst.BackendName, Backend: be},
			Key:         intent.StorageKey,
			Body:        src.Body.Reader(),
			Size:        src.Size,
			ContentType: src.contentType,
			Metadata:    src.metadata,
		})
		if err != nil {
			return fmt.Errorf("write rewritten copy to %s: %w", dst.BackendName, err)
		}
		return nil
	})
	return err
}

// sourceBound reports whether err is about the copy read rather than the
// rewrite, so another copy may succeed where it failed. An encoding that
// misses the ratio and a layer this orchestrator cannot undo or apply end the
// same way whichever copy is read.
func sourceBound(err error) bool {
	if _, ok := errors.AsType[*storedform.Declined](err); ok {
		return false
	}
	return !errors.Is(err, storedform.ErrNoCodec) && !errors.Is(err, storedform.ErrNoEncryptor)
}
