// -------------------------------------------------------------------------------
// Object Manager - HEAD
//
// Author: Alex Freidah
//
// HeadObject orchestration: per-attempt timeout, usage-limit gating, and
// plaintext-size rewrite for encrypted objects. Drives readpath.Failover
// the same way GetObject does but with no streaming body to keep alive.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"fmt"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	pobserve "github.com/afreidah/s3-orchestrator/internal/proxy/observe"
	"github.com/afreidah/s3-orchestrator/internal/proxy/readpath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// HeadObject retrieves object metadata. Tries the primary copy first, then
// falls back to replicas if the primary fails. When the object is encrypted,
// the reported size reflects the original plaintext size.
func (o *Manager) HeadObject(ctx context.Context, key string) (*s3be.HeadObjectResult, error) {
	result, backendName, err := readpath.Read(ctx, o.failover, "HeadObject", key,
		func(ctx context.Context, beName string, loc *core.ObjectLocation, backend s3be.ObjectBackend) (readpath.ProbeResult[*s3be.HeadObjectResult], error) {
			var fail readpath.ProbeResult[*s3be.HeadObjectResult]
			if !o.core.Usage().WithinLimits(beName, 1, 0, 0) {
				return fail, fmt.Errorf("backend %s: %w", beName, readpath.ErrUsageLimitSkip)
			}
			r, err := o.core.HeadWithTimeout(ctx, backend, key)
			if err != nil {
				o.core.Acct().APICall(beName) // API call was made even on failure
				return fail, err
			}

			// Return plaintext size for encrypted objects
			if loc != nil && loc.Encrypted {
				r.Size = loc.PlaintextSize
			}

			// Backends that report no modification time leave LastModified zero,
			// which the transport then drops from the response entirely. Fall back
			// to the stored creation time so every object carries a valid
			// Last-Modified (clients like Tempo's blocklist poller reject an empty
			// one and fail the whole listing).
			if r.LastModified.IsZero() && loc != nil {
				r.LastModified = loc.CreatedAt
			}

			// HEAD carries no streaming body, so a losing result has nothing to
			// release; Cleanup is a no-op.
			return readpath.ProbeResult[*s3be.HeadObjectResult]{
				Value:   r,
				Size:    r.Size,
				Cleanup: readpath.NoopCleanup,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	o.core.Acct().APICall(backendName)

	pobserve.HeadCompleted(ctx, key, backendName, result.Size)
	return result, nil
}
