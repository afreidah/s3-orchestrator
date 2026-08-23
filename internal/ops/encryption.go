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
	"errors"
	"io"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// bulkRewriteBatchSize is how many locations one listing page of a bulk
// rewrite pass collects, bounding how long any single transaction runs.
const bulkRewriteBatchSize = 100

// rotateBatchSize is how many encrypted locations one key-rotation page
// collects.
const rotateBatchSize = 500

// BulkRewriteResult reports one bulk rewrite pass. Total counts every copy
// considered, so Total - Succeeded - Failed - Skipped is zero for a run that
// completed.
//
// Skipped is separate from Failed because a copy can be left alone on purpose:
// a compression pass declines objects too small or too incompressible to be
// worth encoding, and reporting those as failures would make a healthy run look
// broken.
type BulkRewriteResult struct {
	Succeeded int
	Failed    int
	Skipped   int
	Total     int
}

// RotateKeyResult reports one key-rotation pass.
type RotateKeyResult struct {
	Rotated int
	Failed  int
	Total   int
}

// EncryptionDeps holds the collaborators Encryption requires.
type EncryptionDeps struct {
	Encryptor  *encryption.Encryptor
	Store      EncryptionStore
	Runtime    RuntimeOps
	BackendOps BackendOps
}

// Encryption serves the fleet-wide encryption operations. Encryptor and Store
// are nil when the orchestrator was started without encryption, which every
// operation reports as ErrEncryptionDisabled.
type Encryption struct {
	log        *slog.Logger
	encryptor  *encryption.Encryptor
	store      EncryptionStore
	runtime    RuntimeOps
	backendOps BackendOps
}

// NewEncryption is the explicit-deps constructor.
func NewEncryption(d EncryptionDeps) *Encryption {
	must.NotNil("d.Runtime", d.Runtime)
	must.NotNil("d.BackendOps", d.BackendOps)
	return &Encryption{
		log:        slog.Default().With(logfmt.Component("ops")),
		encryptor:  d.Encryptor,
		store:      d.Store,
		runtime:    d.Runtime,
		backendOps: d.BackendOps,
	}
}

// EncryptExisting reads every plaintext copy, encrypts it, re-uploads the
// ciphertext, and records the new encryption metadata.
func (e *Encryption) EncryptExisting(ctx context.Context) (BulkRewriteResult, error) {
	if e.encryptor == nil || e.store == nil {
		return BulkRewriteResult{}, ErrEncryptionDisabled
	}
	return runBulkRewrite(e.rewriteEnv(), ctx, bulkRewriteOp[*encryptRow]{
		opName:      "encrypt-existing",
		resultLabel: "encrypted",
		counter:     telemetry.EncryptExistingObjectsTotal,
		listFn: func(ctx context.Context, batchSize, offset int) ([]*encryptRow, error) {
			rows, err := e.store.ListUnencryptedLocations(ctx, batchSize, offset)
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
					return e.store.MarkObjectEncrypted(ctx, loc.ObjectKey, loc.BackendName, keyData, encResult.KeyID, loc.SizeBytes, encResult.CiphertextSize)
				},
			}, nil
		},
	})
}

// DecryptExisting reads every encrypted copy, decrypts it, re-uploads the
// plaintext, and clears the encryption metadata. Encryption must still be
// configured, since the key provider is what unwraps each DEK.
func (e *Encryption) DecryptExisting(ctx context.Context) (BulkRewriteResult, error) {
	if e.encryptor == nil || e.store == nil {
		return BulkRewriteResult{}, ErrEncryptionDisabled
	}
	return runBulkRewrite(e.rewriteEnv(), ctx, bulkRewriteOp[*decryptRow]{
		opName:      "decrypt-existing",
		resultLabel: "decrypted",
		counter:     telemetry.DecryptExistingObjectsTotal,
		listFn: func(ctx context.Context, batchSize, offset int) ([]*decryptRow, error) {
			rows, err := e.store.ListAllEncryptedLocations(ctx, batchSize, offset)
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
	})
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

// bulkRewriteRow is the minimum surface every row passed to the bulk-rewrite
// driver must expose. Both core.UnencryptedLocation and
// core.DecryptableLocation satisfy it through the wrappers below.
type bulkRewriteRow interface {
	rewriteKey() string
	rewriteBackend() string
	rewriteSize() int64
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

// bulkRewriteOp parameterises one direction of the rewrite: the listing query,
// the transform, and the labels the run reports under.
type bulkRewriteOp[L bulkRewriteRow] struct {
	opName      string
	resultLabel string
	counter     *prometheus.CounterVec
	listFn      func(ctx context.Context, batchSize, offset int) ([]L, error)
	// declines reports a row the pass will not rewrite, before anything is
	// downloaded for it. Only what the row itself says can be judged here; a
	// question about the bytes has to wait for the transform. Nil means every
	// listed row is attempted.
	declines func(loc L) bool
	// rewrite consumes a downloaded object and produces what to upload in its
	// place. Implementations close the source body if they fail before
	// returning. Returning errSkipRewrite leaves the object exactly as it is
	// and counts it as skipped.
	rewrite func(ctx context.Context, src *s3be.GetObjectResult, loc L) (rewritten, error)
}

// rewritten is one transformed object: the bytes to upload, the size to declare
// for them, the metadata update to run once they are durable, and the release
// for anything the transform had to buffer.
//
// release is nil for a transform that streams. It runs whatever the upload
// does, because a pass that leaks a buffer per failed object is a pass that
// fills a disk partway through a fleet.
type rewritten struct {
	body    io.Reader
	size    int64
	commit  func() error
	release func()
}

// errSkipRewrite reports an object a pass declines to rewrite, as opposed to
// one it tried and could not. Only the transform can raise it, because whether
// a rewrite is worth doing is a question about the object's own bytes.
var errSkipRewrite = errors.New("rewrite declined")

// bulkRewriteEnv is what the shared driver needs from whichever service is
// running the pass. Declared so the driver serves the compression passes as
// well as the encryption ones rather than being tied to either.
type bulkRewriteEnv struct {
	log        *slog.Logger
	runtime    RuntimeOps
	backendOps BackendOps
}

// rewriteEnv exposes this service's collaborators to the shared driver.
func (e *Encryption) rewriteEnv() bulkRewriteEnv {
	return bulkRewriteEnv{log: e.log, runtime: e.runtime, backendOps: e.backendOps}
}

// rewriteOutcome is what one object's rewrite attempt produced.
type rewriteOutcome int

const (
	rewriteDone rewriteOutcome = iota
	rewriteSkipped
	rewriteErrored
)

// runBulkRewrite is the shared driver for every bulk rewrite pass. A listing
// failure stops the run and returns the counts gathered so far alongside the
// error, so a caller can report partial progress.
func runBulkRewrite[L bulkRewriteRow](env bulkRewriteEnv, ctx context.Context, op bulkRewriteOp[L]) (BulkRewriteResult, error) {
	var res BulkRewriteResult
	for offset := 0; ; offset += bulkRewriteBatchSize {
		rows, err := op.listFn(ctx, bulkRewriteBatchSize, offset)
		if err != nil {
			env.log.ErrorContext(ctx, op.opName+" list failed", "error", err)
			return res, err
		}
		if len(rows) == 0 {
			break
		}

		for _, row := range rows {
			res.Total++
			switch processBulkLocation(env, ctx, op, row) {
			case rewriteDone:
				res.Succeeded++
			case rewriteSkipped:
				res.Skipped++
			case rewriteErrored:
				res.Failed++
			}
		}

		if len(rows) < bulkRewriteBatchSize {
			break
		}
	}

	env.log.InfoContext(ctx, op.opName+" complete",
		op.resultLabel, res.Succeeded, "skipped", res.Skipped, "failed", res.Failed, "total", res.Total)
	return res, nil
}

// processBulkLocation runs one rewrite step for a single location: download
// from the backend, transform, re-upload, record usage, then update metadata.
// Failures are logged and counted rather than returned, so one bad object does
// not end the pass.
func processBulkLocation[L bulkRewriteRow](env bulkRewriteEnv, ctx context.Context, op bulkRewriteOp[L], loc L) rewriteOutcome {
	key, backendName, sizeBytes := loc.rewriteKey(), loc.rewriteBackend(), loc.rewriteSize()

	if op.declines != nil && op.declines(loc) {
		op.counter.WithLabelValues("skipped").Inc()
		return rewriteSkipped
	}

	be, err := env.runtime.GetBackend(backendName)
	if err != nil {
		return rewriteFailed(env, ctx, op, "backend not found", key, backendName, err)
	}

	src, err := be.GetObject(ctx, key, "")
	if err != nil {
		env.backendOps.RecordUsage(backendName, 1, 0, 0)
		return rewriteFailed(env, ctx, op, "download failed", key, backendName, err)
	}
	env.backendOps.RecordUsage(backendName, 1, sizeBytes, 0)

	out, err := op.rewrite(ctx, src, loc)
	if err != nil {
		src.Body.Close()
		if errors.Is(err, errSkipRewrite) {
			op.counter.WithLabelValues("skipped").Inc()
			return rewriteSkipped
		}
		return rewriteFailed(env, ctx, op, "transform failed", key, backendName, err)
	}
	if out.release != nil {
		defer out.release()
	}

	_, err = be.PutObject(ctx, key, out.body, out.size, src.ContentType, src.Metadata)
	src.Body.Close()
	if err != nil {
		env.backendOps.RecordUsage(backendName, 1, 0, 0)
		return rewriteFailed(env, ctx, op, "re-upload failed", key, backendName, err)
	}
	env.backendOps.RecordUsage(backendName, 1, 0, out.size)

	if err := out.commit(); err != nil {
		return rewriteFailed(env, ctx, op, "metadata update failed", key, backendName, err)
	}

	op.counter.WithLabelValues("success").Inc()
	return rewriteDone
}

// rewriteFailed records one non-fatal rewrite failure. A free function rather
// than a method, since it is parameterised by row type.
func rewriteFailed[L bulkRewriteRow](env bulkRewriteEnv, ctx context.Context, op bulkRewriteOp[L], msg, key, backendName string, err error) rewriteOutcome {
	env.log.WarnContext(ctx, op.opName+": "+msg, "key", key, "backend", backendName, "error", err)
	op.counter.WithLabelValues("error").Inc()
	return rewriteErrored
}
