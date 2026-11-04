// -------------------------------------------------------------------------------
// Ops - Encryption Operations
//
// Author: Alex Freidah
//
// Fleet-wide transitions between plaintext and ciphertext, plus key rotation.
// Encrypt-existing and decrypt-existing differ only in the listing query and
// the form each copy should end up in, so both drive the bulk rewrite driver's
// one pagination and per-copy move. Rotation re-wraps each object's DEK with
// the current primary key without touching the bytes.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"errors"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// rotateBatchSize is how many encrypted locations one key-rotation page
// collects.
const rotateBatchSize = 500

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// EncryptionDeps holds the collaborators Encryption requires.
type EncryptionDeps struct {
	Encryptor *encryption.Encryptor
	Store     RewriteStore
	Mover     Mover
}

// Encryption serves the fleet-wide encryption operations. Encryptor and Store
// are nil when the orchestrator was started without encryption, which every
// operation reports as ErrEncryptionDisabled.
type Encryption struct {
	d   EncryptionDeps
	log *slog.Logger
}

// NewEncryption is the explicit-deps constructor.
func NewEncryption(d EncryptionDeps) *Encryption {
	must.NotNil("d.Mover", d.Mover)
	return &Encryption{d: d, log: logfmt.Logger(logComponent)}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// EncryptExisting rewrites every plaintext copy as ciphertext and records the
// new encryption metadata. A compressed copy keeps its encoding underneath.
//
// maxRewrites caps how many copies are rewritten, or zero for the whole fleet;
// a capped run can be repeated to continue.
func (e *Encryption) EncryptExisting(ctx context.Context, obs progress.Observer, maxRewrites int, backend string) (BulkRewriteResult, error) {
	if e.disabled() {
		return BulkRewriteResult{}, ErrEncryptionDisabled
	}
	op := bulkRewriteOp{
		opName:      "encrypt-existing",
		resultLabel: "encrypted",
		counter:     telemetry.EncryptExistingObjectsTotal,
		layer:       core.StoredEncrypted,
		add:         true,
		filter:      core.FormFilter{Backend: backend},
		maxRewrites: maxRewrites,
	}
	return op.run(ctx, e.rewriteEnv(), obs)
}

// DecryptExisting rewrites every encrypted copy as plaintext and clears the
// encryption metadata. Encryption must still be configured, since the key
// provider is what unwraps each DEK. A compressed copy keeps its encoding.
//
// maxRewrites caps how many copies are rewritten, or zero for the whole fleet;
// a capped run can be repeated to continue.
func (e *Encryption) DecryptExisting(ctx context.Context, obs progress.Observer, maxRewrites int, backend string) (BulkRewriteResult, error) {
	if e.disabled() {
		return BulkRewriteResult{}, ErrEncryptionDisabled
	}
	op := bulkRewriteOp{
		opName:      "decrypt-existing",
		resultLabel: "decrypted",
		counter:     telemetry.DecryptExistingObjectsTotal,
		layer:       core.StoredEncrypted,
		filter:      core.FormFilter{Backend: backend},
		maxRewrites: maxRewrites,
	}
	return op.run(ctx, e.rewriteEnv(), obs)
}

// RotateKey re-wraps DEKs under the current primary key: every DEK sealed with
// oldKeyID, or with allStale every DEK recorded under any other ID, which also
// moves rows still carrying an earlier release's positional IDs onto the
// key's fingerprint. Exactly one of the two must be given. The old keys must
// remain configured, since unwrapping happens with them. Object bytes are
// untouched. A copy a newer write replaced mid-pass counts as skipped.
func (e *Encryption) RotateKey(ctx context.Context, oldKeyID string, allStale bool) (batch.Summary, error) {
	if e.disabled() {
		return batch.Summary{}, ErrEncryptionDisabled
	}
	if (oldKeyID != "") == allStale {
		return batch.Summary{}, ErrKeyIDRequired
	}
	filter := core.FormFilter{With: core.StoredEncrypted, KeyID: oldKeyID}
	if allStale {
		filter.ExceptKeyID = e.d.Encryptor.Provider().KeyID()
	}

	pager := batch.Pager[core.ObjectLocation, core.Cursor]{
		PageSize: batch.FixedPage(rotateBatchSize),
		List: func(ctx context.Context, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
			return e.d.Store.ListCopiesByForm(ctx, &filter, limit, after)
		},
		CursorOf: core.CursorOf,
	}
	res, _, err := pager.RunPages(ctx, batch.Runner[core.ObjectLocation]{Concurrency: 1},
		func(ctx context.Context, loc core.ObjectLocation) batch.ItemResult {
			return e.rotateOneLocation(ctx, &loc)
		})
	if err != nil {
		return res, err
	}

	e.log.InfoContext(ctx, "key rotation complete", "rotated", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
	return res, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// rotateOneLocation re-wraps the DEK for a single encrypted location. Every
// failure mode is non-fatal so the pass continues. A copy a newer write
// replaced mid-pass is skipped: the newer write sealed it under the current
// key already.
func (e *Encryption) rotateOneLocation(ctx context.Context, loc *core.ObjectLocation) batch.ItemResult {
	baseNonce, wrappedDEK, unpackErr := encryption.UnpackKeyData(loc.EncryptionKey)
	if unpackErr != nil {
		return e.rotateFailed(ctx, "unpack failed", loc.ObjectKey, unpackErr)
	}

	dek, unwrapErr := e.d.Encryptor.Provider().UnwrapDEK(ctx, wrappedDEK, loc.KeyID)
	if unwrapErr != nil {
		return e.rotateFailed(ctx, "unwrap failed", loc.ObjectKey, unwrapErr)
	}

	newWrapped, newKeyID, wrapErr := e.d.Encryptor.Provider().WrapDEK(ctx, dek)
	if wrapErr != nil {
		return e.rotateFailed(ctx, "wrap failed", loc.ObjectKey, wrapErr)
	}

	err := e.d.Store.UpdateCopyMetadata(ctx, loc, &core.CopyPatch{
		EncryptionKey: encryption.PackKeyData(baseNonce, newWrapped),
		KeyID:         newKeyID,
	})
	if errors.Is(err, core.ErrCopyChanged) {
		telemetry.KeyRotationObjectsTotal.WithLabelValues("changed").Inc()
		return batch.ItemResult{Outcome: batch.ItemSkipped}
	}
	if err != nil {
		return e.rotateFailed(ctx, "update failed", loc.ObjectKey, err)
	}

	telemetry.KeyRotationObjectsTotal.WithLabelValues("success").Inc()
	return batch.Succeeded()
}

// rotateFailed records one non-fatal rotation failure, so each failure path in
// rotateOneLocation stays a single line.
func (e *Encryption) rotateFailed(ctx context.Context, msg, key string, err error) batch.ItemResult {
	e.log.WarnContext(ctx, "key rotation "+msg, "key", key, "error", err)
	telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
	return batch.Failed()
}

// rewriteEnv exposes this service's collaborators to the shared driver. A
// pass changes only the envelope, so an encoding is carried through rather
// than decoded and no codec is needed.
func (e *Encryption) rewriteEnv() bulkRewriteEnv {
	return bulkRewriteEnv{log: e.log, store: e.d.Store, mover: e.d.Mover, layers: &storedform.Layers{Encryptor: e.d.Encryptor}}
}

// disabled reports whether the orchestrator runs without encryption, which
// every operation here refuses with ErrEncryptionDisabled.
func (e *Encryption) disabled() bool {
	return e.d.Encryptor == nil || e.d.Store == nil
}
