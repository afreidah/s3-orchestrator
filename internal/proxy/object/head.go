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
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	pobserve "github.com/afreidah/s3-orchestrator/internal/proxy/observe"
	"github.com/afreidah/s3-orchestrator/internal/proxy/readpath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// HeadObject retrieves object metadata. An object whose row carries its
// identity is answered from the ledger alone, with no backend call. Otherwise it
// tries the primary copy, then the replicas, and records what the backend
// reported so the next HEAD can skip the backend. The reported size is the size
// the client wrote, even when the stored bytes are encrypted or compressed.
func (o *Manager) HeadObject(ctx context.Context, key string) (*HeadResult, error) {
	locs := o.locationsForHead(ctx, key)
	if res, ok := o.headFromMetadata(ctx, key, locs); ok {
		return res, nil
	}

	result, backendName, err := o.failover.Read(ctx, "HeadObject", key,
		func(ctx context.Context, beName string, loc *core.ObjectLocation, backend s3be.ObjectBackend) (readpath.ProbeResult[*s3be.HeadObjectResult], error) {
			var fail readpath.ProbeResult[*s3be.HeadObjectResult]
			if !o.core.Usage().WithinLimits(beName, headObjectOp, 0, 0) {
				return fail, fmt.Errorf("backend %s: %w", beName, readpath.ErrUsageLimitSkip)
			}
			loc, err := o.readLocation(ctx, backend, beName, key, loc)
			if err != nil {
				return fail, err
			}
			// HEAD has no body to inspect, so a contradictory row is the only
			// divergence it can see - but it is the one that matters here,
			// since the size reported below is read straight off that row.
			// Failing over beats answering with a ciphertext size.
			if err := core.ValidateEncryptionMetadata(loc); err != nil {
				telemetry.EncryptionFlagMismatchTotal.WithLabelValues("head").Inc()
				return fail, fmt.Errorf("backend %s: %w", beName, err)
			}

			r, err := o.core.HeadWithTimeout(ctx, backend, core.StoragePath(key, loc.StorageKey))
			if err != nil {
				o.core.Acct().APICall(s3op.HeadObject, beName) // API call was made even on failure
				return fail, err
			}

			// Report the size the client wrote, not the size on the backend.
			// Those differ once the bytes were encrypted, compressed, or both,
			// and a HEAD that reports the stored size sends clients ranging
			// against coordinates the object does not have.
			if loc.Encrypted || isCompressed(loc) {
				r.Size = logicalSize(loc)
			}

			r.LastModified = resolveLastModified(r.LastModified, loc)

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
	o.core.Acct().APICall(s3op.HeadObject, backendName)

	// Record what the backend reported on every copy of the key. The ETag is
	// adopted only when the stored bytes are the client's bytes; for a
	// compressed or encrypted copy the scrubber fills it in from a plaintext
	// read.
	o.recordHeadIdentity(ctx, key, result, locs)

	pobserve.HeadCompleted(ctx, key, backendName, result.Size)
	return &HeadResult{HeadObjectResult: result, TagCount: tagCountOf(locs)}, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// locationsForHead reads the rows a HEAD needs in one lookup. A store error
// returns no rows, since the backend path repeats the lookup and reports it.
func (o *Manager) locationsForHead(ctx context.Context, key string) []core.ObjectLocation {
	locs, err := core.ClientLocations(o.stores.GetAllObjectLocations(ctx, key))
	if err != nil {
		return nil
	}
	return locs
}

// tagCountOf reports the key's tag count from its location rows, which all
// carry the same value. No rows reports zero, which leaves the tagging-count
// header off.
func tagCountOf(locs []core.ObjectLocation) int {
	if len(locs) == 0 {
		return 0
	}
	return locs[0].TagCount
}

// headFromMetadata answers a HEAD from the rows the caller already read, when
// the first of them carries a complete identity. Reports false when it cannot,
// which leaves the caller on the backend path.
func (o *Manager) headFromMetadata(ctx context.Context, key string, locs []core.ObjectLocation) (*HeadResult, bool) {
	if len(locs) == 0 {
		return nil, false
	}
	loc := &locs[0]
	if !loc.Identity.Complete() {
		return nil, false
	}
	if err := core.ValidateEncryptionMetadata(loc); err != nil {
		telemetry.EncryptionFlagMismatchTotal.WithLabelValues("head").Inc()
		return nil, false
	}

	res := &s3be.HeadObjectResult{
		Size:         logicalSize(loc),
		ContentType:  loc.Identity.ContentType,
		ETag:         loc.Identity.ETag,
		LastModified: loc.CreatedAt,
		Metadata:     loc.Identity.UserMetadata,
	}
	if !loc.Encrypted && !isCompressed(loc) {
		res.Size = loc.SizeBytes
	}
	telemetry.HeadServedFromMetadataTotal.Inc()
	pobserve.HeadCompleted(ctx, key, "metadata", res.Size)
	return &HeadResult{HeadObjectResult: res, TagCount: loc.TagCount}, true
}

// recordHeadIdentity persists what a backend HEAD reported so the next one is
// answered locally. Best effort: a write failure costs another round trip
// later, which is what the call just did anyway.
func (o *Manager) recordHeadIdentity(ctx context.Context, key string, r *s3be.HeadObjectResult, locs []core.ObjectLocation) {
	id := &core.ObjectIdentity{
		ContentType:  r.ContentType,
		UserMetadata: r.Metadata,
	}
	if id.UserMetadata == nil {
		id.UserMetadata = map[string]string{}
	}
	if storedBytesAreClientBytes(locs) {
		id.ETag = r.ETag
	}
	if !fillsMissingColumn(id, locs) {
		return
	}
	if err := o.stores.RecordObjectIdentity(ctx, key, id); err != nil {
		o.log.WarnContext(ctx, "failed to record object identity", "key", key, "error", err)
	}
}

// fillsMissingColumn reports whether recording id would fill a column some copy
// lacks; the write only fills NULLs and never overwrites. Without the check, a
// key whose ETag the backend cannot supply (compressed or encrypted) would
// rewrite every copy on every HEAD. No rows means nothing to fill.
func fillsMissingColumn(id *core.ObjectIdentity, locs []core.ObjectLocation) bool {
	for i := range locs {
		stored := locs[i].Identity
		if stored == nil || stored.UserMetadata == nil {
			return true
		}
		if id.ETag != "" && stored.ETag == "" {
			return true
		}
		if id.ContentType != "" && stored.ContentType == "" {
			return true
		}
	}
	return false
}

// storedBytesAreClientBytes reports whether every copy of the key is stored
// verbatim, which is what makes a backend's ETag the object's ETag. A copy
// that is compressed or encrypted disqualifies the key: the ETag is a property
// of the object, so adopting one that describes a stored form would publish it
// for the copies it does not describe either.
func storedBytesAreClientBytes(locs []core.ObjectLocation) bool {
	if len(locs) == 0 {
		return false
	}
	for i := range locs {
		if locs[i].Encrypted || isCompressed(&locs[i]) {
			return false
		}
	}
	return true
}
