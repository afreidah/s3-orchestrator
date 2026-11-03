// -------------------------------------------------------------------------------
// Backend - Copy From Any Source
//
// Author: Alex Freidah
//
// Walks the stored copies of one object until a stream copy from one of them
// lands on the destination. Replication and CopyObject's stream-through path
// both read through it, so a source that fails is skipped the same way.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// CopyRunner resolves source backends and sends the stream copy.
// *infra.BackendRuntime satisfies it.
type CopyRunner interface {
	GetBackend(name string) (ObjectBackend, error)
	StreamCopy(ctx context.Context, req *CopyRequest) (string, error)
}

// SourcedCopy copies the object Key to DstKey on Dst from whichever of Sources
// reads first. OnReadFail, when set, sees each source whose read failed before
// the next one is tried.
type SourcedCopy struct {
	Key        string
	Sources    []core.ObjectLocation
	Dst        Endpoint
	DstKey     string
	OnReadFail func(src *core.ObjectLocation, err error)
}

// ErrNoSourceRead reports a copy none of whose sources could be read.
var ErrNoSourceRead = errors.New("no source copy could be read")

// CopyFromAny tries each source in order and returns the one the copy read
// from, with the ETag the destination reported. A write failure ends the walk,
// since every other source would be written to the same destination.
func CopyFromAny(ctx context.Context, rt CopyRunner, c *SourcedCopy) (*core.ObjectLocation, string, error) {
	var lastErr error
	for i := range c.Sources {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		src := &c.Sources[i]
		be, err := rt.GetBackend(src.BackendName)
		if err != nil {
			continue
		}
		etag, err := rt.StreamCopy(ctx, &CopyRequest{
			Src:     Endpoint{Name: src.BackendName, Backend: be},
			Dst:     c.Dst,
			SrcKey:  core.StoragePath(c.Key, src.StorageKey),
			DstKey:  c.DstKey,
			SrcSize: src.SizeBytes,
		})
		if err == nil {
			return src, etag, nil
		}
		if IsCopyPhase(err, CopyPhaseWrite) {
			return nil, "", err
		}
		lastErr = err
		if c.OnReadFail != nil {
			c.OnReadFail(src, err)
		}
	}
	if lastErr == nil {
		return nil, "", fmt.Errorf("%w for %s", ErrNoSourceRead, c.Key)
	}
	return nil, "", fmt.Errorf("%w for %s: %w", ErrNoSourceRead, c.Key, lastErr)
}
