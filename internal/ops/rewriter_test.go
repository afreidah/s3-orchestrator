// -------------------------------------------------------------------------------
// Ops - Fake Rewriter
//
// Author: Alex Freidah
//
// A rewrite pass hands each object's copies to the Rewriter, which reads one,
// reforms its bytes and places the result on every copy's backend. This fake
// does the same steps against the backends it is given, so a pass's layers run
// over real bytes, and it records what it was asked and what it placed. A
// configured placement error stands in for the store declining or failing the
// placement.
// -------------------------------------------------------------------------------

package ops

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// placedCopy is one copy the fake rewriter recorded: the copy it replaced, the
// form and size of the bytes it wrote, and those bytes.
type placedCopy struct {
	source core.ObjectLocation
	form   core.StoredForm
	size   int64
	body   []byte
}

// fakeRewriter rewrites copies on the named backends the way the write
// coordinator does, without a store: it reads the first copy, reforms it
// through the request's layers, and writes the result to each copy's backend.
// A name missing from backends fails that read or write. placeErr, when set,
// is returned after the bytes are written, as a placement the store declined
// or failed. onPlaced runs for each placement that succeeded.
type fakeRewriter struct {
	mu       sync.Mutex
	backends map[string]s3be.ObjectBackend
	placeErr error
	onPlaced func(loc *core.ObjectLocation)
	reqs     []writepath.RewriteRequest
	placed   []placedCopy
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// RewriteCopies reforms the request's first copy and lands the result on
// every copy, returning one error per copy.
func (r *fakeRewriter) RewriteCopies(ctx context.Context, req *writepath.RewriteRequest) []error {
	r.mu.Lock()
	r.reqs = append(r.reqs, *req)
	r.mu.Unlock()

	errs := make([]error, len(req.Copies))
	out, err := r.reform(ctx, req)
	if err != nil {
		for i := range errs {
			errs[i] = err
		}
		return errs
	}
	defer out.Release()
	for i := range req.Copies {
		errs[i] = r.land(ctx, &req.Copies[i], out)
	}
	return errs
}

// placements returns a snapshot of the copies placed so far.
func (r *fakeRewriter) placements() []placedCopy {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]placedCopy(nil), r.placed...)
}

// requests returns a snapshot of every request the rewriter was handed.
func (r *fakeRewriter) requests() []writepath.RewriteRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]writepath.RewriteRequest(nil), r.reqs...)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// reform reads the request's first copy and reforms it into the form wanted.
func (r *fakeRewriter) reform(ctx context.Context, req *writepath.RewriteRequest) (*storedform.Reformed, error) {
	src := &req.Copies[0]
	be, ok := r.backends[src.BackendName]
	if !ok {
		return nil, fmt.Errorf("read %s: backend not found", src.BackendName)
	}
	read, err := be.GetObject(ctx, src.ObjectKey, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = read.Body.Close() }()
	return req.Layers.Reform(ctx, read.Body, read.Size, &src.StoredForm, req.Want, nil)
}

// land writes the reformed bytes to dst's backend and records the placement.
func (r *fakeRewriter) land(ctx context.Context, dst *core.ObjectLocation, out *storedform.Reformed) error {
	be, ok := r.backends[dst.BackendName]
	if !ok {
		return fmt.Errorf("write %s: backend not found", dst.BackendName)
	}
	data, err := io.ReadAll(out.Body.Reader())
	if err != nil {
		return err
	}
	if _, err := be.PutObject(ctx, dst.ObjectKey, bytes.NewReader(data), out.Size, "", nil); err != nil {
		return err
	}
	if r.placeErr != nil {
		return r.placeErr
	}

	r.mu.Lock()
	r.placed = append(r.placed, placedCopy{source: *dst, form: *out.Form, size: out.Size, body: data})
	onPlaced := r.onPlaced
	r.mu.Unlock()
	if onPlaced != nil {
		onPlaced(dst)
	}
	return nil
}
