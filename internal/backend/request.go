// -------------------------------------------------------------------------------
// Backend - Runtime Call Requests
//
// Author: Alex Freidah
//
// The requests the backend runtime sends on a caller's behalf. Each names the
// backend it reaches by the name its usage is charged to, so the runtime can
// apply the timeout and charge the call where it is sent rather than at every
// call site.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"io"
)

// Endpoint pairs a backend client with the name its usage is charged to, so a
// call can be checked against and charged to that backend at the point the
// bytes move.
type Endpoint struct {
	Name    string
	Backend ObjectBackend
}

// PutRequest is one PUT: Size bytes of Body written at Key on Dst, carrying the
// content type and user metadata the stored object keeps.
type PutRequest struct {
	Dst         Endpoint
	Key         string
	Body        io.Reader
	Size        int64
	ContentType string
	Metadata    map[string]string
}

// CopyRequest is one backend-to-backend copy: read SrcKey from Src and write
// DstKey on Dst. SizeEstimate is what both legs are admitted against before
// any byte moves. Transform, when set, rewrites the bytes between the legs;
// nil copies them verbatim.
type CopyRequest struct {
	Src, Dst       Endpoint
	SrcKey, DstKey string
	SizeEstimate   int64
	Transform      CopyTransform
}

// CopyTransform turns the object a copy read into the bytes it writes,
// returning the body, its size and a release for anything it buffered. The
// release runs once the write is done, whatever its outcome.
type CopyTransform func(ctx context.Context, src *GetObjectResult) (body io.Reader, size int64, release func(), err error)
