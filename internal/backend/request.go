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
	"errors"
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

// CopyRequest is one backend-to-backend copy of the bytes verbatim: read
// SrcKey from Src and write DstKey on Dst. SrcSize is the size the source's row
// records: both legs are admitted against it, and a source read of any other
// size fails the copy.
type CopyRequest struct {
	Src, Dst       Endpoint
	SrcKey, DstKey string
	SrcSize        int64
}

// ErrSourceSizeMismatch reports a copy source whose stored bytes are not the
// size its row records, such as a truncated or empty object.
var ErrSourceSizeMismatch = errors.New("source size does not match its row")
