// -------------------------------------------------------------------------------
// Ops - Fake Mover
//
// Author: Alex Freidah
//
// A rewrite pass hands each copy to the Mover, which reads it, reforms it on
// the way and places the result. This fake does the same steps against the
// test's backends, so a pass runs its Reform closure over real bytes, and it
// records what it was asked and what it placed. A configured placement error
// stands in for the store declining or failing the placement.
// -------------------------------------------------------------------------------

package ops

import (
	"bytes"
	"context"
	"io"
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// placedCopy is one copy the fake mover recorded: the copy it moved, the form
// and size of the bytes it wrote, and those bytes.
type placedCopy struct {
	source core.ObjectLocation
	form   core.StoredForm
	size   int64
	body   []byte
}

// fakeMover moves copies between the request's backends the way the write
// coordinator does, without a store. placeErr, when set, is returned after the
// bytes are written, as a placement the store declined or failed. onPlaced
// runs for each placement that succeeded.
type fakeMover struct {
	mu       sync.Mutex
	placeErr error
	onPlaced func(loc *core.ObjectLocation)
	requests []writepath.MoveRequest
	placed   []placedCopy
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// MoveObject reads the source copy, reforms it when the request says how,
// writes it to the destination, and records the placement.
func (m *fakeMover) MoveObject(ctx context.Context, req *writepath.MoveRequest) (int64, error) {
	m.mu.Lock()
	m.requests = append(m.requests, *req)
	m.mu.Unlock()

	src, err := req.SrcBackend.GetObject(ctx, req.Source.ObjectKey, "")
	if err != nil {
		return 0, err
	}
	defer func() { _ = src.Body.Close() }()

	body, size, form := io.Reader(src.Body), src.Size, req.Source.StoredForm
	if req.Reform != nil {
		out, err := req.Reform(ctx, src)
		if err != nil {
			return 0, err
		}
		defer out.Release()
		body, size, form = out.Body, out.Size, *out.Form
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return 0, err
	}
	if _, err := req.DestBackend.PutObject(ctx, req.Source.ObjectKey, bytes.NewReader(data), size, "", nil); err != nil {
		return 0, err
	}
	if m.placeErr != nil {
		return 0, m.placeErr
	}

	m.mu.Lock()
	m.placed = append(m.placed, placedCopy{source: *req.Source, form: form, size: size, body: data})
	onPlaced := m.onPlaced
	m.mu.Unlock()
	if onPlaced != nil {
		onPlaced(req.Source)
	}
	return size, nil
}

// placements returns a snapshot of the copies placed so far.
func (m *fakeMover) placements() []placedCopy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]placedCopy(nil), m.placed...)
}

// moveRequests returns a snapshot of every request the mover was handed.
func (m *fakeMover) moveRequests() []writepath.MoveRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]writepath.MoveRequest(nil), m.requests...)
}
