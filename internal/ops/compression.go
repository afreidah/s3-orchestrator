// -------------------------------------------------------------------------------
// Ops - Compression Operations
//
// Author: Alex Freidah
//
// Fleet-wide transitions between stored-verbatim and stored-encoded. Enabling
// compression only affects objects written afterwards, so these are what make
// the feature adoptable on a fleet that already holds data - and what takes it
// back out again.
//
// Both directions drive the same pagination and per-copy move the encryption
// passes use, differing only in the listing query and the form each copy
// should end up in. Compression sits inside encryption, so an encrypted copy
// is unsealed before its bytes are encoded or decoded and sealed again after.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"errors"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// CompressionDeps holds the collaborators Compression requires.
type CompressionDeps struct {
	Codec     storedform.Codec
	Config    config.CompressionConfig
	Encryptor *encryption.Encryptor
	Store     RewriteStore
	Runtime   RuntimeOps
	Mover     Mover
}

// Compression serves the fleet-wide compression operations. Codec and Store are
// nil when the orchestrator was started without them, which every operation
// reports as ErrCompressionUnavailable.
//
// Encryptor may be nil, in which case encrypted copies are refused.
type Compression struct {
	log     *slog.Logger
	cfg     config.CompressionConfig
	store   RewriteStore
	runtime RuntimeOps
	mover   Mover
	layers  *storedform.Layers
}

// NewCompression is the explicit-deps constructor.
func NewCompression(d *CompressionDeps) *Compression {
	must.NotNil("d.Runtime", d.Runtime)
	must.NotNil("d.Mover", d.Mover)
	return &Compression{
		log:     slog.Default().With(logfmt.Component("ops")),
		cfg:     d.Config,
		store:   d.Store,
		runtime: d.Runtime,
		mover:   d.Mover,
		layers:  &storedform.Layers{Encryptor: d.Encryptor, Codec: d.Codec, Compression: d.Config},
	}
}

// rewriteEnv exposes this service's collaborators to the shared driver.
func (c *Compression) rewriteEnv() bulkRewriteEnv {
	return bulkRewriteEnv{log: c.log, runtime: c.runtime, mover: c.mover, layers: c.layers}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// CompressExisting encodes every copy stored verbatim and records the new
// stored form, applying the same thresholds a PUT does. Objects that do not
// shrink past the configured ratio are left as they are and counted as skipped.
//
// maxRewrites caps how many copies are rewritten, or zero for the whole fleet.
// Rewritten and declined copies both leave the listing, so running a capped
// pass again continues with the next batch.
func (c *Compression) CompressExisting(ctx context.Context, obs progress.Observer, maxRewrites int, backend string) (BulkRewriteResult, error) {
	if c.layers.Codec == nil || c.store == nil {
		return BulkRewriteResult{}, ErrCompressionUnavailable
	}
	return bulkRewriteOp{
		opName:      "compress-existing",
		resultLabel: "compressed",
		counter:     telemetry.CompressExistingObjectsTotal,
		// The size floor and the recorded declines are both applied by the
		// listing rather than here: both answers outlive the pass, so a copy
		// either one excludes selects out of every future pass instead of being
		// handed to each one only to be declined again.
		listFn: func(ctx context.Context, batchSize int, after core.Cursor) ([]core.ObjectLocation, error) {
			return c.store.ListCopiesByForm(ctx, &core.FormFilter{
				Without: core.StoredCompressed,
				Backend: backend,
				Candidates: &core.CompressionThresholds{
					MinSize:  c.cfg.MinSize,
					MinRatio: c.cfg.MinRatio,
					Level:    c.cfg.Level,
				},
			}, batchSize, after)
		},
		want:        func(loc *core.ObjectLocation) core.StoredFlags { return loc.Form() | core.StoredCompressed },
		onDeclined:  c.recordDecline,
		maxRewrites: maxRewrites,
	}.run(ctx, c.rewriteEnv(), obs)
}

// DecompressExisting decodes every encoded copy and records it as stored
// verbatim. maxRewrites caps how many copies are rewritten, or zero for the
// whole fleet; a capped run can be repeated to continue.
func (c *Compression) DecompressExisting(ctx context.Context, obs progress.Observer, maxRewrites int, backend string) (BulkRewriteResult, error) {
	if c.layers.Codec == nil || c.store == nil {
		return BulkRewriteResult{}, ErrCompressionUnavailable
	}
	return bulkRewriteOp{
		opName:      "decompress-existing",
		resultLabel: "decompressed",
		counter:     telemetry.DecompressExistingObjectsTotal,
		listFn: func(ctx context.Context, batchSize int, after core.Cursor) ([]core.ObjectLocation, error) {
			return c.store.ListCopiesByForm(ctx, &core.FormFilter{With: core.StoredCompressed, Backend: backend}, batchSize, after)
		},
		want:        func(loc *core.ObjectLocation) core.StoredFlags { return loc.Form() &^ core.StoredCompressed },
		maxRewrites: maxRewrites,
	}.run(ctx, c.rewriteEnv(), obs)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// recordDecline records an encoding that missed the ratio, so later runs skip
// this copy instead of re-encoding it. A copy a newer write replaced has
// nothing to record against.
func (c *Compression) recordDecline(ctx context.Context, loc *core.ObjectLocation, encodedSize int64) {
	err := c.store.UpdateCopyMetadata(ctx, loc, &core.CopyPatch{Probe: &core.CompressionProbe{
		Size:  encodedSize,
		Level: c.cfg.Level,
	}})
	if err != nil && !errors.Is(err, core.ErrCopyChanged) {
		c.log.WarnContext(ctx, "failed to record compression probe",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
	}
}
