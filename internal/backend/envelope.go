// -------------------------------------------------------------------------------
// Ranged Reads and the Envelope Header
//
// Author: Alex Freidah
//
// The ranged read a caller with no database row uses to learn how stored bytes
// are encoded, and the read of the fixed-size encryption header off the front
// of an object. Each caller supplies the reader, so the read is charged to
// whatever meters that caller holds.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"fmt"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/util/byterange"
)

// RangeReader reads bytes start through end of the object stored at key.
type RangeReader interface {
	ReadRange(ctx context.Context, key string, start, end int64) ([]byte, error)
}

// DirectReader reads ranges straight from a backend with no timeout and no
// charge, for a caller that holds no usage counters.
type DirectReader struct {
	Backend ObjectBackend
}

// ReadRange implements RangeReader.
func (d DirectReader) ReadRange(ctx context.Context, key string, start, end int64) ([]byte, error) {
	r, err := d.Backend.GetObject(ctx, key, byterange.Header(start, end))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Body.Close() }()
	return ReadSpan(r.Body, start, end)
}

// ReadSpan reads at most the span start through end from a ranged GET's body,
// so a backend that ignores Range sends the whole object but only the span is
// read.
func ReadSpan(body io.Reader, start, end int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, end-start+1))
}

// ReadEnvelopeHeader reads the leading encryption-header bytes of key. An
// object too small to hold a header yields fewer bytes, which the caller reads
// as "not an envelope", and the 416 a zero-byte object answers with reads as
// an empty header.
func ReadEnvelopeHeader(ctx context.Context, r RangeReader, key string) ([]byte, error) {
	hdr, err := r.ReadRange(ctx, key, 0, encryption.HeaderSize-1)
	if isRangeNotSatisfiable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read object header: %w", err)
	}
	return hdr, nil
}
