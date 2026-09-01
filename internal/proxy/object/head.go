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
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// HeadObject retrieves object metadata. An object whose row carries its
// identity is answered from the ledger alone - no backend round trip and no
// metered API call, which is the whole point of storing it. Everything else
// tries the primary copy first, then falls back to replicas, and records what
// the backend reported so the next HEAD does not have to ask.
//
// When the object is encrypted, the reported size reflects the original
// plaintext size.
func (o *Manager) HeadObject(ctx context.Context, key string) (*HeadResult, error) {
	locs := o.locationsForHead(ctx, key)
	if res, ok := o.headFromMetadata(ctx, key, locs); ok {
		return res, nil
	}

	result, backendName, err := o.failover.Read(ctx, "HeadObject", key,
		func(ctx context.Context, beName string, loc *core.ObjectLocation, backend s3be.ObjectBackend) (readpath.ProbeResult[*s3be.HeadObjectResult], error) {
			var fail readpath.ProbeResult[*s3be.HeadObjectResult]
			if !o.core.Usage().WithinLimits(beName, 1, 0, 0) {
				return fail, fmt.Errorf("backend %s: %w", beName, readpath.ErrUsageLimitSkip)
			}
			// HEAD has no body to inspect, so a contradictory row is the only
			// divergence it can see - but it is the one that matters here,
			// since the size reported below is read straight off that row.
			// Failing over beats answering with a ciphertext size.
			if err := core.ValidateEncryptionMetadata(loc); err != nil {
				telemetry.EncryptionFlagMismatchTotal.WithLabelValues("head").Inc()
				return fail, fmt.Errorf("backend %s: %w", beName, err)
			}

			r, err := o.core.HeadWithTimeout(ctx, backend, key)
			if err != nil {
				o.core.Acct().APICall(beName) // API call was made even on failure
				return fail, err
			}

			// Report the size the client wrote, not the size on the backend.
			// Those differ once the bytes were encrypted, compressed, or both,
			// and a HEAD that reports the stored size sends clients ranging
			// against coordinates the object does not have.
			if loc != nil && (loc.Encrypted || isCompressed(loc)) {
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
	o.core.Acct().APICall(backendName)

	// What the backend just reported is what the next HEAD would spend another
	// call to learn, so it is recorded on every copy of the key. The ETag is
	// only adopted when the stored bytes are the client's bytes: for a
	// compressed or encrypted copy the backend's value is a digest of the
	// stored form, and the scrubber fills that one in from a plaintext read.
	o.recordHeadIdentity(ctx, key, result, locs)

	pobserve.HeadCompleted(ctx, key, backendName, result.Size)
	return &HeadResult{HeadObjectResult: result, TagCount: o.countObjectTags(ctx, key)}, nil
}

// locationsForHead reads the rows a HEAD needs, once: the identity that may
// answer it outright, and the copies that decide what a backend answer is
// worth recording. Reading them three times is what this call replaces.
//
// A store error yields no rows rather than an error: the backend path is about
// to run the same lookup and will report it, and a HEAD that can be served
// either way should not fail on the cheaper attempt.
func (o *Manager) locationsForHead(ctx context.Context, key string) []core.ObjectLocation {
	locs, err := o.stores.GetAllObjectLocations(ctx, key)
	if err != nil {
		return nil
	}
	return locs
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
	return &HeadResult{HeadObjectResult: res, TagCount: o.countObjectTags(ctx, key)}, true
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

// fillsMissingColumn reports whether recording id would put a value where a
// copy has none, which is the only thing the write does: it fills NULLs and
// never overwrites. Without the check, a key whose ETag a backend cannot
// supply - a compressed or encrypted one, whose ETag the scrubber owns - would
// rewrite every copy on every HEAD for the rest of its life.
//
// No rows means nothing to fill: the caller could not read the ledger, and the
// next HEAD records what this one skipped.
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
