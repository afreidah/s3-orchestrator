// -------------------------------------------------------------------------------
// Ops - Encryption Operations
//
// Author: Alex Freidah
//
// Fleet-wide transitions between plaintext and ciphertext, plus key rotation.
// Encrypt-existing and decrypt-existing differ only in the listing query and
// the transform applied to each object, so both drive one pagination,
// download, transform, re-upload and metadata-update loop. Rotation re-wraps
// each object's DEK with the current primary key without touching the bytes.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"log/slog"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
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

// RotateKeyResult reports one key-rotation pass.
type RotateKeyResult struct {
	Rotated int
	Failed  int
	Total   int
}

// EncryptionDeps holds the collaborators Encryption requires.
type EncryptionDeps struct {
	Encryptor *encryption.Encryptor
	Store     EncryptionStore
	Runtime   RuntimeOps
	Usage     UsageGate
}

// Encryption serves the fleet-wide encryption operations. Encryptor and Store
// are nil when the orchestrator was started without encryption, which every
// operation reports as ErrEncryptionDisabled.
type Encryption struct {
	log       *slog.Logger
	encryptor *encryption.Encryptor
	store     EncryptionStore
	runtime   RuntimeOps
	usage     UsageGate
}

// NewEncryption is the explicit-deps constructor.
func NewEncryption(d EncryptionDeps) *Encryption {
	must.NotNil("d.Runtime", d.Runtime)
	must.NotNil("d.Usage", d.Usage)
	return &Encryption{
		log:       slog.Default().With(logfmt.Component("ops")),
		encryptor: d.Encryptor,
		store:     d.Store,
		runtime:   d.Runtime,
		usage:     d.Usage,
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// EncryptExisting reads every plaintext copy, encrypts it, re-uploads the
// ciphertext, and records the new encryption metadata.
//
// maxRewrites caps how many copies are rewritten, or zero for the whole fleet. A
// capped run needs nothing carried between invocations to continue: an encrypted
// copy leaves the listing that selected it, so running it again converts the
// next batch rather than re-examining the last.
func (e *Encryption) EncryptExisting(ctx context.Context, obs progress.Observer, maxRewrites int) (BulkRewriteResult, error) {
	if e.encryptor == nil || e.store == nil {
		return BulkRewriteResult{}, ErrEncryptionDisabled
	}
	return bulkRewriteOp[*encryptRow]{
		opName:      "encrypt-existing",
		resultLabel: "encrypted",
		counter:     telemetry.EncryptExistingObjectsTotal,
		maxRewrites: maxRewrites,
		listFn: func(ctx context.Context, batchSize int, after core.Cursor) ([]*encryptRow, error) {
			rows, err := e.store.ListUnencryptedLocations(ctx, batchSize, after)
			if err != nil {
				return nil, err
			}
			out := make([]*encryptRow, len(rows))
			for i := range rows {
				out[i] = &encryptRow{rows[i]}
			}
			return out, nil
		},
		rewrite: func(ctx context.Context, src *s3be.GetObjectResult, loc *encryptRow) (rewritten, error) {
			encResult, err := e.encryptor.Encrypt(ctx, src.Body, loc.SizeBytes)
			if err != nil {
				return rewritten{}, err
			}
			return rewritten{
				body: encResult.Body,
				size: encResult.CiphertextSize,
				commit: func() error {
					keyData := encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK)
					return e.store.MarkObjectEncrypted(ctx, &core.EncryptedUpdate{
						ObjectKey:      loc.ObjectKey,
						BackendName:    loc.BackendName,
						EncryptionKey:  keyData,
						KeyID:          encResult.KeyID,
						PlaintextSize:  loc.SizeBytes,
						CiphertextSize: encResult.CiphertextSize,
					})
				},
			}, nil
		},
	}.run(ctx, e.rewriteEnv(), obs)
}

// DecryptExisting reads every encrypted copy, decrypts it, re-uploads the
// plaintext, and clears the encryption metadata. Encryption must still be
// configured, since the key provider is what unwraps each DEK.
//
// maxRewrites caps how many copies are rewritten, or zero for the whole fleet.
// This direction declines nothing, so every copy a capped run touches leaves the
// listing and the next run continues straight on from there.
func (e *Encryption) DecryptExisting(ctx context.Context, obs progress.Observer, maxRewrites int) (BulkRewriteResult, error) {
	if e.encryptor == nil || e.store == nil {
		return BulkRewriteResult{}, ErrEncryptionDisabled
	}
	return bulkRewriteOp[*decryptRow]{
		opName:      "decrypt-existing",
		resultLabel: "decrypted",
		counter:     telemetry.DecryptExistingObjectsTotal,
		maxRewrites: maxRewrites,
		listFn: func(ctx context.Context, batchSize int, after core.Cursor) ([]*decryptRow, error) {
			rows, err := e.store.ListAllEncryptedLocations(ctx, batchSize, after)
			if err != nil {
				return nil, err
			}
			out := make([]*decryptRow, len(rows))
			for i := range rows {
				out[i] = &decryptRow{rows[i]}
			}
			return out, nil
		},
		rewrite: func(ctx context.Context, src *s3be.GetObjectResult, loc *decryptRow) (rewritten, error) {
			plainReader, plainLen, err := e.encryptor.DecryptStored(ctx, src.Body, loc.EncryptionKey, loc.KeyID, loc.PlaintextSize, nil)
			if err != nil {
				return rewritten{}, err
			}
			return rewritten{
				body: plainReader,
				size: plainLen,
				commit: func() error {
					return e.store.MarkObjectDecrypted(ctx, loc.ObjectKey, loc.BackendName, loc.PlaintextSize)
				},
			}, nil
		},
	}.run(ctx, e.rewriteEnv(), obs)
}

// RotateKey re-wraps every DEK sealed with oldKeyID under the current primary
// key. The old key must remain in previous_keys, since unwrapping happens with
// it. Object bytes are untouched.
func (e *Encryption) RotateKey(ctx context.Context, oldKeyID string) (RotateKeyResult, error) {
	if e.encryptor == nil || e.store == nil {
		return RotateKeyResult{}, ErrEncryptionDisabled
	}
	if oldKeyID == "" {
		return RotateKeyResult{}, ErrKeyIDRequired
	}

	var res RotateKeyResult
	for offset := 0; ; offset += rotateBatchSize {
		locs, err := e.store.ListEncryptedLocations(ctx, oldKeyID, rotateBatchSize, offset)
		if err != nil {
			return res, err
		}
		e.rotateBatch(ctx, locs, &res)
		if len(locs) < rotateBatchSize {
			break
		}
	}

	e.log.InfoContext(ctx, "key rotation complete", "rotated", res.Rotated, "failed", res.Failed, "total", res.Total)
	return res, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// rotateBatch re-wraps one page of locations and folds the outcomes into res.
func (e *Encryption) rotateBatch(ctx context.Context, locs []core.EncryptedLocation, res *RotateKeyResult) {
	for _, loc := range locs {
		if e.rotateOneLocation(ctx, loc) {
			res.Rotated++
		} else {
			res.Failed++
		}
	}
	res.Total += len(locs)
}

// rotateOneLocation re-wraps the DEK for a single encrypted location. Reports
// success; every failure mode is non-fatal so the pass continues.
func (e *Encryption) rotateOneLocation(ctx context.Context, loc core.EncryptedLocation) bool {
	baseNonce, wrappedDEK, unpackErr := encryption.UnpackKeyData(loc.EncryptionKey)
	if unpackErr != nil {
		return e.rotateFailed(ctx, "unpack failed", loc.ObjectKey, unpackErr)
	}

	dek, unwrapErr := e.encryptor.Provider().UnwrapDEK(ctx, wrappedDEK, loc.KeyID)
	if unwrapErr != nil {
		return e.rotateFailed(ctx, "unwrap failed", loc.ObjectKey, unwrapErr)
	}

	newWrapped, newKeyID, wrapErr := e.encryptor.Provider().WrapDEK(ctx, dek)
	if wrapErr != nil {
		return e.rotateFailed(ctx, "wrap failed", loc.ObjectKey, wrapErr)
	}

	newKeyData := encryption.PackKeyData(baseNonce, newWrapped)
	if err := e.store.UpdateEncryptionKey(ctx, loc.ObjectKey, loc.BackendName, newKeyData, newKeyID); err != nil {
		return e.rotateFailed(ctx, "update failed", loc.ObjectKey, err)
	}

	telemetry.KeyRotationObjectsTotal.WithLabelValues("success").Inc()
	return true
}

// rotateFailed records one non-fatal rotation failure and reports false, so
// each failure path in rotateOneLocation stays a single line.
func (e *Encryption) rotateFailed(ctx context.Context, msg, key string, err error) bool {
	e.log.WarnContext(ctx, "key rotation "+msg, "key", key, "error", err)
	telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
	return false
}

// encryptRow adapts a plaintext location to bulkRewriteRow. Pointer receivers
// avoid copying the embedded store row.
type encryptRow struct{ core.UnencryptedLocation }

// rewriteKey returns the object key to re-process.
func (r *encryptRow) rewriteKey() string { return r.ObjectKey }

// rewriteBackend returns the backend the row currently lives on.
func (r *encryptRow) rewriteBackend() string { return r.BackendName }

// rewriteSize returns the row's stored size, used for quota accounting.
func (r *encryptRow) rewriteSize() int64 { return r.SizeBytes }

// decryptRow adapts an encrypted location to bulkRewriteRow, so one driver
// serves both directions of the rewrite.
type decryptRow struct{ core.DecryptableLocation }

// rewriteKey returns the object key to re-process.
func (r *decryptRow) rewriteKey() string { return r.ObjectKey }

// rewriteBackend returns the backend the row currently lives on.
func (r *decryptRow) rewriteBackend() string { return r.BackendName }

// rewriteSize returns the row's stored size, used for quota accounting.
func (r *decryptRow) rewriteSize() int64 { return r.SizeBytes }

// rewriteEnv exposes this service's collaborators to the shared driver.
func (e *Encryption) rewriteEnv() bulkRewriteEnv {
	return bulkRewriteEnv{log: e.log, runtime: e.runtime, usage: e.usage}
}
