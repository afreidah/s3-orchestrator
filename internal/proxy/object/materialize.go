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

// materializeCopySource reads the source object from the first reachable copy,
// in location order, into a seekable buffer ready for PutObject. When every
// copy fails it returns the last underlying error, such as DeadlineExceeded,
// rather than a generic one.
func (o *Manager) materializeCopySource(
	ctx context.Context,
	sourceKey string,
	size int64,
	locations []core.ObjectLocation,
) (*materializedSource, error) {
	var lastErr error
	for i := range locations {
		ms, err := o.tryMaterializeFromLocation(ctx, core.StoragePath(sourceKey, locations[i].StorageKey), size, locations[i].BackendName)
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

// tryMaterializeFromLocation downloads one copy at storageKey into a fresh
// seekable buffer. It returns (nil, nil) when the copy was skipped without a
// hard error (usage limit reached or backend not registered), and (nil, err)
// when the GET or the buffering failed.
func (o *Manager) tryMaterializeFromLocation(
	ctx context.Context,
	storageKey string,
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
	result, cancel, err := o.core.GetWithTimeout(ctx, be, storageKey, "")
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
