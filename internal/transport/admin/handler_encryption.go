// -------------------------------------------------------------------------------
// Admin API - Encryption Key Rotation and Bulk Encrypt/Decrypt
//
// Author: Alex Freidah
//
// Key rotation re-wraps every encrypted object's DEK with the current
// primary key (the old key must remain in previous_keys for unwrapping).
// The bulk encrypt-existing / decrypt-existing endpoints share one
// pagination + download + transform + upload + DB-update driver
// (runBulkRewriteCounts) parameterised over a typed row interface.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// errEncryptionNotEnabled is the body emitted by every encryption-aware
// endpoint when the orchestrator was started without an encryptor.
const errEncryptionNotEnabled = "encryption not enabled"

// handleRotateEncryptionKey re-wraps all encrypted objects' DEKs with
// the current primary key. Objects are processed in batches to avoid
// holding long transactions. The old key must remain in previous_keys
// for unwrapping.
func (h *Handler) handleRotateEncryptionKey(w http.ResponseWriter, r *http.Request) {
	if h.encryptor == nil || h.encAdmin == nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, errEncryptionNotEnabled)
		return
	}

	var req adminapi.RotateEncryptionKeyRequest
	if !httputil.DecodeJSONBody(w, r, &req, 1<<20) {
		return
	}
	if req.OldKeyID == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "old_key_id is required")
		return
	}

	ctx := r.Context()
	const batchSize = 500
	var rotated, failed, total int

	for offset := 0; ; offset += batchSize {
		locs, err := h.encAdmin.ListEncryptedLocations(ctx, req.OldKeyID, batchSize, offset)
		if err != nil {
			h.log.ErrorContext(ctx, "key rotation list failed", "error", err)
			httputil.WriteJSONError(w, http.StatusInternalServerError, "failed to list encrypted objects")
			return
		}
		if len(locs) == 0 {
			break
		}
		batchRotated, batchFailed := h.rotateBatch(ctx, locs)
		rotated += batchRotated
		failed += batchFailed
		total += len(locs)
		if len(locs) < batchSize {
			break
		}
	}

	h.log.InfoContext(ctx, "key rotation complete", "rotated", rotated, "failed", failed, "total", total)
	httputil.WriteJSON(w, http.StatusOK, adminapi.RotateEncryptionKeyResponse{
		BulkEncryptionOutcome: adminapi.BulkEncryptionOutcome{
			Status: "complete",
			Failed: failed,
			Total:  total,
		},
		Rotated: rotated,
	})
}

// rotateBatch re-wraps the DEKs for one paginated batch of encrypted
// locations, returning the per-batch success and failure counts.
func (h *Handler) rotateBatch(ctx context.Context, locs []core.EncryptedLocation) (rotated, failed int) {
	for _, loc := range locs {
		if h.rotateOneLocation(ctx, loc) {
			rotated++
		} else {
			failed++
		}
	}
	return rotated, failed
}

// rotateOneLocation re-wraps the DEK for a single encrypted location.
// Returns true on success; logs and increments error telemetry on
// failure (every failure mode is non-fatal so the batch can continue).
func (h *Handler) rotateOneLocation(ctx context.Context, loc core.EncryptedLocation) bool {
	baseNonce, wrappedDEK, unpackErr := encryption.UnpackKeyData(loc.EncryptionKey)
	if unpackErr != nil {
		h.log.WarnContext(ctx, "unpack failed", slog.String("key", loc.ObjectKey), "error", unpackErr)
		telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
		return false
	}

	dek, unwrapErr := h.encryptor.Provider().UnwrapDEK(ctx, wrappedDEK, loc.KeyID)
	if unwrapErr != nil {
		h.log.WarnContext(ctx, "unwrap failed", "key", loc.ObjectKey, "error", unwrapErr)
		telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
		return false
	}

	newWrapped, newKeyID, wrapErr := h.encryptor.Provider().WrapDEK(ctx, dek)
	if wrapErr != nil {
		h.log.WarnContext(ctx, "wrap failed", "key", loc.ObjectKey, "error", wrapErr)
		telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
		return false
	}

	newKeyData := encryption.PackKeyData(baseNonce, newWrapped)
	if err := h.encAdmin.UpdateEncryptionKey(ctx, loc.ObjectKey, loc.BackendName, newKeyData, newKeyID); err != nil {
		h.log.WarnContext(ctx, "update failed", "key", loc.ObjectKey, "error", err)
		telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
		return false
	}

	telemetry.KeyRotationObjectsTotal.WithLabelValues("success").Inc()
	return true
}

// -------------------------------------------------------------------------
// BULK ENCRYPT / DECRYPT EXISTING OBJECTS
// -------------------------------------------------------------------------

// bulkRewriteRow is the minimum surface every row passed to the bulk-rewrite
// driver must expose. Both store.UnencryptedLocation and
// store.DecryptableLocation satisfy it.
type bulkRewriteRow interface {
	rewriteKey() string
	rewriteBackend() string
	rewriteSize() int64
}

// adapter wrappers (unexported) keep the handlers free of accessor noise
// while letting bulkRewriteRow stay independent of the store package types.
// Pointer receivers avoid copying the embedded store row (DecryptableLocation
// carries a []byte and trips gocritic's hugeParam).
type encryptRow struct{ core.UnencryptedLocation }

// rewriteKey returns the object key the bulk-rewrite loop should
// re-process. Implements the rewriteRow interface for encryptRow.
func (r *encryptRow) rewriteKey() string { return r.ObjectKey }

// rewriteBackend returns the source backend the row currently lives on.
// Implements the rewriteRow interface for encryptRow.
func (r *encryptRow) rewriteBackend() string { return r.BackendName }

// rewriteSize returns the row's stored size, used for quota accounting
// and progress reporting. Implements rewriteRow for encryptRow.
func (r *encryptRow) rewriteSize() int64 { return r.SizeBytes }

// decryptRow wraps core.DecryptableLocation so the same bulkRewriteRow
// machinery that handles encrypt-existing can run decrypt-existing
// without duplicating the pagination + download + upload + DB-update
// scaffolding.
type decryptRow struct{ core.DecryptableLocation }

// rewriteKey returns the object key to re-process. Implements
// rewriteRow for decryptRow.
func (r *decryptRow) rewriteKey() string { return r.ObjectKey }

// rewriteBackend returns the source backend the row currently lives on.
// Implements rewriteRow for decryptRow.
func (r *decryptRow) rewriteBackend() string { return r.BackendName }

// rewriteSize returns the row's stored size, used for quota accounting
// and progress reporting. Implements rewriteRow for decryptRow.
func (r *decryptRow) rewriteSize() int64 { return r.SizeBytes }

// bulkRewriteOp parameterises the encrypt/decrypt-existing handlers, which
// share their pagination + download + upload + DB-update scaffolding and
// differ only in the listing query, the transform step, and the result
// label.
type bulkRewriteOp[L bulkRewriteRow] struct {
	opName      string // "encrypt-existing" / "decrypt-existing"
	logTag      string // "Encrypt-existing" / "Decrypt-existing"
	listErrMsg  string
	resultLabel string // "encrypted" / "decrypted"
	counter     *prometheus.CounterVec
	listFn      func(ctx context.Context, batchSize, offset int) ([]L, error)
	// rewrite consumes a downloaded object and returns the bytes to re-upload,
	// the size to record as PUT egress, and a closure that performs the DB
	// metadata update on success. Implementations are responsible for closing
	// the source body if they fail before returning the new reader.
	rewrite func(ctx context.Context, src *backend.GetObjectResult, loc L) (io.Reader, int64, func() error, error)
}

// BulkRewriteResult is the outcome of a bulk encrypt/decrypt-existing pass.
// Status is "complete" when the run finished, "skipped" when the operation
// is unavailable (encryption not enabled).
type BulkRewriteResult struct {
	Status  string
	Reason  string
	Success int
	Failed  int
	Total   int
}

// EncryptExisting downloads every unencrypted object, encrypts it,
// re-uploads the ciphertext, and updates the DB record. Returns counts.
// Skips when encryption is not configured.
func (h *Handler) EncryptExisting(ctx context.Context) BulkRewriteResult {
	return runBulkRewriteCounts(h, ctx, bulkRewriteOp[*encryptRow]{
		opName:      "encrypt-existing",
		logTag:      "Encrypt-existing",
		listErrMsg:  "failed to list unencrypted objects",
		resultLabel: "encrypted",
		counter:     telemetry.EncryptExistingObjectsTotal,
		listFn: func(ctx context.Context, batchSize, offset int) ([]*encryptRow, error) {
			rows, err := h.encAdmin.ListUnencryptedLocations(ctx, batchSize, offset)
			if err != nil {
				return nil, err
			}
			out := make([]*encryptRow, len(rows))
			for i := range rows {
				out[i] = &encryptRow{rows[i]}
			}
			return out, nil
		},
		rewrite: func(ctx context.Context, src *backend.GetObjectResult, loc *encryptRow) (io.Reader, int64, func() error, error) {
			encResult, err := h.encryptor.Encrypt(ctx, src.Body, loc.SizeBytes)
			if err != nil {
				return nil, 0, nil, err
			}
			dbUpdate := func() error {
				keyData := encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK)
				return h.encAdmin.MarkObjectEncrypted(ctx, loc.ObjectKey, loc.BackendName, keyData, encResult.KeyID, loc.SizeBytes, encResult.CiphertextSize)
			}
			return encResult.Body, encResult.CiphertextSize, dbUpdate, nil
		},
	})
}

// handleEncryptExisting wraps EncryptExisting in JSON envelope semantics.
func (h *Handler) handleEncryptExisting(w http.ResponseWriter, r *http.Request) {
	res := h.EncryptExisting(r.Context())
	if res.Status == "skipped" {
		httputil.WriteJSONError(w, http.StatusBadRequest, res.Reason)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.EncryptExistingResponse{
		BulkEncryptionOutcome: adminapi.BulkEncryptionOutcome{
			Status: "complete",
			Failed: res.Failed,
			Total:  res.Total,
		},
		Encrypted: res.Success,
	})
}

// handleDecryptExisting downloads each encrypted object from its backend,
// decrypts it, re-uploads the plaintext, and updates the DB record. Objects
// are processed in batches to avoid holding long transactions. Encryption
// must still be configured (the key provider is needed to unwrap DEKs).
func (h *Handler) handleDecryptExisting(w http.ResponseWriter, r *http.Request) {
	res := runBulkRewriteCounts(h, r.Context(), bulkRewriteOp[*decryptRow]{
		opName:      "decrypt-existing",
		logTag:      "Decrypt-existing",
		listErrMsg:  "failed to list encrypted objects",
		resultLabel: "decrypted",
		counter:     telemetry.DecryptExistingObjectsTotal,
		listFn: func(ctx context.Context, batchSize, offset int) ([]*decryptRow, error) {
			rows, err := h.encAdmin.ListAllEncryptedLocations(ctx, batchSize, offset)
			if err != nil {
				return nil, err
			}
			out := make([]*decryptRow, len(rows))
			for i := range rows {
				out[i] = &decryptRow{rows[i]}
			}
			return out, nil
		},
		rewrite: func(ctx context.Context, src *backend.GetObjectResult, loc *decryptRow) (io.Reader, int64, func() error, error) {
			plainReader, plainLen, err := h.encryptor.DecryptStored(ctx, src.Body, loc.EncryptionKey, loc.KeyID, loc.PlaintextSize, nil)
			if err != nil {
				return nil, 0, nil, err
			}
			dbUpdate := func() error {
				return h.encAdmin.MarkObjectDecrypted(ctx, loc.ObjectKey, loc.BackendName, loc.PlaintextSize)
			}
			return plainReader, plainLen, dbUpdate, nil
		},
	})
	if res.Status == "skipped" {
		httputil.WriteJSONError(w, http.StatusBadRequest, res.Reason)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.DecryptExistingResponse{
		BulkEncryptionOutcome: adminapi.BulkEncryptionOutcome{
			Status: "complete",
			Failed: res.Failed,
			Total:  res.Total,
		},
		Decrypted: res.Success,
	})
}

// runBulkRewriteCounts is the shared driver for the encrypt/decrypt-existing
// operations. Returns counts so callers (HTTP handlers, UI handlers, tests)
// can decide how to surface them. A list-listFn error short-circuits the
// run with the partial counts gathered so far.
func runBulkRewriteCounts[L bulkRewriteRow](h *Handler, ctx context.Context, op bulkRewriteOp[L]) BulkRewriteResult {
	if h.encryptor == nil || h.encAdmin == nil {
		return BulkRewriteResult{Status: "skipped", Reason: errEncryptionNotEnabled}
	}

	const batchSize = 100
	var success, failed, total int

	for offset := 0; ; offset += batchSize {
		rows, err := op.listFn(ctx, batchSize, offset)
		if err != nil {
			h.log.ErrorContext(ctx, "Admin: "+op.opName+" list failed", "error", err)
			return BulkRewriteResult{Status: "skipped", Reason: op.listErrMsg, Success: success, Failed: failed, Total: total}
		}
		if len(rows) == 0 {
			break
		}

		for _, row := range rows {
			total++
			if processBulkLocation(h, ctx, op, row) {
				success++
			} else {
				failed++
			}
		}

		if len(rows) < batchSize {
			break
		}
	}

	h.log.InfoContext(ctx, "Admin: "+op.opName+" complete", op.resultLabel, success, "failed", failed, "total", total)
	return BulkRewriteResult{Status: "complete", Success: success, Failed: failed, Total: total}
}

// processBulkLocation runs one rewrite step for a single object location:
// download from backend, transform via op.rewrite, upload, record usage,
// and run the DB update. Returns true on success, false on any failure
// (which is already logged and metric-recorded).
func processBulkLocation[L bulkRewriteRow](h *Handler, ctx context.Context, op bulkRewriteOp[L], loc L) bool {
	key, backendName, sizeBytes := loc.rewriteKey(), loc.rewriteBackend(), loc.rewriteSize()

	be, err := h.runtimeOps.GetBackend(backendName)
	if err != nil {
		h.log.WarnContext(ctx, op.logTag+": backend not found", "key", key, "backend", backendName, "error", err)
		op.counter.WithLabelValues("error").Inc()
		return false
	}

	src, err := be.GetObject(ctx, key, "")
	if err != nil {
		h.backendOps.RecordUsage(backendName, 1, 0, 0)
		h.log.WarnContext(ctx, op.logTag+": download failed", "key", key, "backend", backendName, "error", err)
		op.counter.WithLabelValues("error").Inc()
		return false
	}
	h.backendOps.RecordUsage(backendName, 1, sizeBytes, 0)

	body, putSize, dbUpdate, err := op.rewrite(ctx, src, loc)
	if err != nil {
		src.Body.Close()
		h.log.WarnContext(ctx, op.logTag+": transform failed", "key", key, "error", err)
		op.counter.WithLabelValues("error").Inc()
		return false
	}

	_, err = be.PutObject(ctx, key, body, putSize, src.ContentType, src.Metadata)
	src.Body.Close()
	if err != nil {
		h.backendOps.RecordUsage(backendName, 1, 0, 0)
		h.log.WarnContext(ctx, op.logTag+": re-upload failed", "key", key, "backend", backendName, "error", err)
		op.counter.WithLabelValues("error").Inc()
		return false
	}
	h.backendOps.RecordUsage(backendName, 1, 0, putSize)

	if err := dbUpdate(); err != nil {
		h.log.WarnContext(ctx, op.logTag+": DB update failed", "key", key, "error", err)
		op.counter.WithLabelValues("error").Inc()
		return false
	}

	op.counter.WithLabelValues("success").Inc()
	return true
}
