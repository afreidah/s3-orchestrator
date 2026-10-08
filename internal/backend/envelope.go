// -------------------------------------------------------------------------------
// Envelope Header Fetch
//
// Author: Alex Freidah
//
// Reads just the fixed-size encryption header off the front of a stored
// object. Used by the import paths, which have to know whether bytes they
// discovered on a backend are an encryption envelope before they record
// metadata claiming otherwise.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// FetchEnvelopeHeader reads the leading encryption-header bytes of key from
// be. Returns fewer bytes than a full header for objects too small to hold
// one, which the caller reads as "not an envelope". A backend that ignores
// Range still works because only the prefix is read, and the 416 a zero-byte
// object answers with is read as an empty header.
func FetchEnvelopeHeader(ctx context.Context, be ObjectBackend, key string) ([]byte, error) {
	r, err := be.GetObject(ctx, key, fmt.Sprintf("bytes=0-%d", encryption.HeaderSize-1))
	if isRangeNotSatisfiable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read object header: %w", err)
	}
	defer func() { _ = r.Body.Close() }()

	hdr := make([]byte, encryption.HeaderSize)
	n, err := io.ReadFull(r.Body, hdr)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("read object header: %w", err)
	}
	return hdr[:n], nil
}
