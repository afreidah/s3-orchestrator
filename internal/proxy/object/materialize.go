// -------------------------------------------------------------------------------
// Copy Source Materialization
//
// Author: Alex Freidah
//
// Reads a CopyObject source object from the first reachable replica into a
// seekable body (via internal/util/materialize) so it can be handed to
// PutObject and replayed across failover attempts. Also holds the small
// SHA-256 helpers the PUT integrity pipeline feeds into the materialize sink.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/materialize"
)

// -------------------------------------------------------------------------
// HASHING
// -------------------------------------------------------------------------

// sha256Hex returns the SHA-256 accumulated in h as a hex string. Convenience
// for call sites that materialize a body with a streaming hasher attached.
func sha256Hex(h hash.Hash) string {
	if h == nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// newSHA256 returns a fresh SHA-256 hasher. Exposed as a helper so call sites
// do not need to import crypto/sha256 just to feed materialize.New's hasher
// parameter.
func newSHA256() hash.Hash {
	return sha256.New()
}

// -------------------------------------------------------------------------
// MATERIALIZED SOURCE
// -------------------------------------------------------------------------

// materializedSource bundles the seekable reader handed to PutObject with the
// cleanup the caller must invoke once the upload settles. The returned source
// backend identifies which replica actually served the bytes so CopyObject can
// attribute usage correctly.
type materializedSource struct {
	body          io.ReadSeeker
	sourceBackend string
	cleanup       func()
}

// materializeCopySource reads the source object from the first reachable
// replica into a seekable buffer (in-memory for small objects, a self-
// unlinking tempfile for large ones) and returns it ready for handoff to
// PutObject. Failover iterates locations in order; backend-side errors
// (including backend-timeout cancellation) are captured and a different
// replica is tried. On total failure the most recent underlying error
// surfaces so the caller sees the real signal (e.g. DeadlineExceeded) rather
// than a generic wrapper. Per-replica GETs run under the backend timeout
// policy; a tighter caller deadline still wins.
func (o *Manager) materializeCopySource(
	ctx context.Context,
	sourceKey string,
	size int64,
	locations []core.ObjectLocation,
) (*materializedSource, error) {
	var lastErr error
	for i := range locations {
		ms, err := o.tryMaterializeFromLocation(ctx, sourceKey, size, locations[i].BackendName)
		if err != nil {
			lastErr = err
			continue
		}
		if ms != nil {
			return ms, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("failed to read source from any copy")
}

// tryMaterializeFromLocation attempts to download sourceKey from one backend
// into a fresh seekable buffer. (ms, nil) on success. (nil, nil) means the
// replica was skipped without a hard error (usage limits hit, backend not
// registered) — caller moves on. (nil, err) is a real failure (backend GET
// errored or materialization failed). Errors are aggregated by the caller so
// the last underlying failure surfaces when no replica succeeds.
func (o *Manager) tryMaterializeFromLocation(
	ctx context.Context,
	sourceKey string,
	size int64,
	backendName string,
) (*materializedSource, error) {
	if !o.core.Usage().WithinLimits(backendName, getObjectOp, size, 0) {
		return nil, nil
	}
	be, ok := o.core.Backends()[backendName]
	if !ok {
		return nil, nil
	}

	// The backend timeout covers the body drain inside materialize.New too:
	// cancel only fires on function return, by which point the body has been
	// fully materialized.
	result, cancel, err := o.core.GetWithTimeout(ctx, be, sourceKey, "")
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer result.Body.Close()

	mb, err := materialize.New(result.Body, size, nil)
	if err != nil {
		return nil, err
	}
	body, err := mb.Reader()
	if err != nil {
		mb.Cleanup()
		return nil, err
	}
	return &materializedSource{
		body:          body,
		sourceBackend: backendName,
		cleanup:       mb.Cleanup,
	}, nil
}
