// -------------------------------------------------------------------------------
// Multipart DEK Backfill Worker
//
// Author: Alex Freidah
//
// Promotes legacy multipart_uploads rows (created before migration 00010, when
// each UploadPart wrapped its own DEK) to the shared-DEK format the runtime
// path now requires. For every row whose encryption_key is NULL: acquire the
// per-uploadID advisory lock so the runtime cannot race the rebuild, wrap a
// fresh DEK once, walk the upload's parts re-encrypting each ciphertext under
// the shared DEK with PUT-overwrite, update each multipart_parts row, and
// finally stamp the multipart_uploads row's encryption_key + key_id so the
// runtime stops treating the upload as legacy.
//
// Idempotency. The upload row's encryption_key is the commit marker; until
// every part has been re-encrypted and recorded, the row stays in legacy
// state. A crash mid-rebuild leaves a row that may have some parts re-encrypted
// (their on-disk bytes) with the part_row metadata pointing at the new DEK,
// while other parts are untouched. Re-running the backfill on that row will
// generate a fresh top-level DEK and try to decrypt every part. Already-
// rebuilt parts will fail to decrypt under the previous run's discarded DEK
// and the upload is logged as unrecoverable. The cleanup-queue / stale-
// multipart sweeper picks the upload up later via Abort. The operator-visible
// outcome is that a multipart upload that was in flight when a backfill crash
// occurred has to be re-uploaded by the client; this is the same contract S3
// already offers via Abort + retry.
//
// Concurrency. Uploads are dispatched to a workerpool so multiple legacy rows
// can rebuild in parallel. Each worker owns its own advisory lock per row, so
// safety vs the runtime path is preserved regardless of pool width.
// -------------------------------------------------------------------------------

// Package worker provides background workers and helpers shared across
// the orchestrator's lifecycle services.
package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// MultipartBackfillStore is the narrow per-role surface the backfill
// worker uses. The DI layer composes it from the existing CB-decorated
// MultipartStore + AdvisoryLocker.
type MultipartBackfillStore interface {
	core.MultipartStore
	core.AdvisoryLocker
}

// MultipartBackfill rebuilds legacy multipart uploads under a shared
// upload-level DEK. It is safe to run on every startup; once every
// row has a non-null encryption_key, the worker becomes a no-op.
type MultipartBackfill struct {
	store     MultipartBackfillStore
	encryptor *encryption.Encryptor
	backends  func(name string) (s3be.ObjectBackend, error)

	pageSize    int
	concurrency int
}

// MultipartBackfillConfig parameterises the backfill worker. Zero
// values fall back to the constants below so callers that do not need
// to override anything can pass a zero-valued config.
type MultipartBackfillConfig struct {
	// PageSize caps the number of legacy rows fetched per database
	// pass. Smaller values keep statement memory bounded; larger
	// values reduce the number of round-trips when the backlog is
	// large.
	PageSize int

	// Concurrency caps how many uploads are rebuilt in parallel. Each
	// upload acquires its own advisory lock so concurrency is safe;
	// the cap exists so the worker does not saturate the backend
	// fleet during a large migration. Default 1.
	Concurrency int
}

// Default tuning constants for the backfill worker.
const (
	defaultBackfillPageSize    = 50
	defaultBackfillConcurrency = 1
)

// NewMultipartBackfill constructs a backfill worker. backends resolves
// a backend by name; the worker uses it to fetch parts and PUT
// rewritten ciphertext.
func NewMultipartBackfill(store MultipartBackfillStore, encryptor *encryption.Encryptor, backends func(name string) (s3be.ObjectBackend, error), cfg MultipartBackfillConfig) *MultipartBackfill {
	pageSize := cfg.PageSize
	if pageSize <= 0 {
		pageSize = defaultBackfillPageSize
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = defaultBackfillConcurrency
	}
	return &MultipartBackfill{
		store:       store,
		encryptor:   encryptor,
		backends:    backends,
		pageSize:    pageSize,
		concurrency: concurrency,
	}
}

// Run walks every page of legacy uploads until the backlog is empty
// or ctx is cancelled. Returns the count of rows successfully migrated
// (the operator-facing audit metric) and the first non-recoverable
// error, if any.
func (b *MultipartBackfill) Run(ctx context.Context) (migrated int, err error) {
	if b.encryptor == nil {
		// No proxy-side encryption configured - no rows can be in
		// the legacy format because the legacy format only exists
		// when encryption is on. Nothing to do.
		return 0, nil
	}
	for {
		if ctx.Err() != nil {
			return migrated, ctx.Err()
		}
		legacy, listErr := b.store.ListLegacyMultipartUploads(ctx, b.pageSize)
		if listErr != nil {
			return migrated, fmt.Errorf("list legacy multipart uploads: %w", listErr)
		}
		if len(legacy) == 0 {
			return migrated, nil
		}

		var page int
		// Index into legacy so the rebuildOne pointer-arg sees a
		// stable address rather than a copied loop value.
		indices := make([]int, len(legacy))
		for i := range legacy {
			indices[i] = i
		}
		workerpool.Run(ctx, b.concurrency, indices, func(ctx context.Context, idx int) {
			mu := &legacy[idx]
			if rebuildErr := b.rebuildOne(ctx, mu); rebuildErr != nil {
				slog.WarnContext(ctx, "multipart_dek_backfill: skipping upload",
					"upload_id", mu.UploadID, logfmt.Err(rebuildErr))
				telemetry.MultipartDEKBackfillTotal.WithLabelValues("error").Inc()
				return
			}
			page++
			telemetry.MultipartDEKBackfillTotal.WithLabelValues("success").Inc()
		})
		migrated += page

		// Defensive: if the page was non-empty but we migrated zero
		// rows AND the backlog still reports the same head, we are
		// looping on uploads we cannot rebuild. Break to avoid a
		// hot-loop and let the operator inspect logs.
		if page == 0 {
			return migrated, nil
		}
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// rebuildOne migrates one upload under its advisory lock. The caller
// already filtered for legacy rows; this function still re-checks
// because a concurrent Complete or Abort could have resolved the row
// between listing and lock acquisition.
func (b *MultipartBackfill) rebuildOne(ctx context.Context, mu *core.MultipartUpload) error {
	acquired, err := b.store.WithAdvisoryLock(ctx, multipartLockID(mu.UploadID), func(ctx context.Context) error {
		return b.rebuildLocked(ctx, mu.UploadID)
	})
	if err != nil {
		return err
	}
	if !acquired {
		// Another caller (likely Complete) holds the lock. Skip and
		// let the next Run pick this row up if it is still legacy.
		return nil
	}
	return nil
}

// rebuildLocked is the body that runs under the advisory lock. It re-
// reads the row to absorb any state changes that landed between the
// Run-level listing and the lock acquisition.
func (b *MultipartBackfill) rebuildLocked(ctx context.Context, uploadID string) error {
	mu, err := b.store.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		if errors.Is(err, core.ErrMultipartUploadNotFound) {
			return nil
		}
		return fmt.Errorf("re-read upload row: %w", err)
	}
	if mu.Encrypted && len(mu.EncryptionKey) > 0 {
		// Already migrated by a prior Run on a different instance.
		return nil
	}
	be, err := b.backends(mu.BackendName)
	if err != nil {
		return fmt.Errorf("resolve backend %q: %w", mu.BackendName, err)
	}

	parts, err := b.store.GetParts(ctx, uploadID)
	if err != nil {
		return fmt.Errorf("get parts: %w", err)
	}

	dek, wrappedDEK, keyID, err := b.encryptor.GenerateAndWrapDEK(ctx)
	if err != nil {
		return fmt.Errorf("wrap upload DEK: %w", err)
	}

	for i := range parts {
		part := &parts[i]
		if err := b.rebuildPart(ctx, be, mu, part, dek, wrappedDEK, keyID); err != nil {
			return fmt.Errorf("part %d: %w", part.PartNumber, err)
		}
	}

	// Pack with a zero base nonce so the upload row matches the
	// runtime CreateMultipartUpload write path; per-part baseNonces
	// live on each multipart_parts row and the assembled object
	// stores its own baseNonce in object_locations.encryption_key.
	uploadKey := encryption.PackKeyData(make([]byte, encryption.NonceSize), wrappedDEK)
	if err := b.store.UpdateUploadEncryption(ctx, uploadID, uploadKey, keyID); err != nil {
		return fmt.Errorf("update upload encryption: %w", err)
	}

	audit.Log(ctx, "multipart_dek_backfill.complete",
		slog.String("upload_id", uploadID),
		slog.String("backend", mu.BackendName),
		slog.Int("parts", len(parts)),
	)
	return nil
}

// rebuildPart streams one part's ciphertext from the backend, decrypts
// with the part's per-part DEK, re-encrypts under the upload-level
// DEK, PUT-overwrites the same key (atomic at the backend), and
// records the new encryption metadata.
//
// The part-row update lands AFTER the PUT so a crash between PUT and
// row-update leaves the part bytes inconsistent with the row's DEK
// reference. The next Run regenerates a different upload-level DEK
// and tries to decrypt every part with its part_row metadata - if the
// part_row still references the legacy DEK but the bytes are under a
// discarded DEK, that part fails to decrypt and the whole rebuild for
// this upload aborts. The operator inspects logs and aborts the
// upload via the admin API; clients re-upload. This is documented
// behaviour because preserving an in-flight upload through a crash
// would require a multi-phase write protocol the part schema does
// not currently express.
func (b *MultipartBackfill) rebuildPart(ctx context.Context, be s3be.ObjectBackend, mu *core.MultipartUpload, part *core.MultipartPart, dek, wrappedDEK []byte, keyID string) error {
	partKey := multipartPartKey(mu.UploadID, part.PartNumber)
	src, err := be.GetObject(ctx, partKey, "")
	if err != nil {
		return fmt.Errorf("get part ciphertext: %w", err)
	}
	defer src.Body.Close() //nolint:errcheck // best-effort close on a streaming source

	plaintext, err := decryptLegacyPart(ctx, b.encryptor, src.Body, part)
	if err != nil {
		return fmt.Errorf("decrypt legacy part: %w", err)
	}

	result, err := b.encryptor.EncryptWithDEK(bytes.NewReader(plaintext), int64(len(plaintext)), dek, wrappedDEK, keyID)
	if err != nil {
		return fmt.Errorf("re-encrypt part: %w", err)
	}
	newCiphertext, err := io.ReadAll(result.Body)
	if err != nil {
		return fmt.Errorf("collect re-encrypted ciphertext: %w", err)
	}
	if _, err := be.PutObject(ctx, partKey, bytes.NewReader(newCiphertext), int64(len(newCiphertext)), "application/octet-stream", nil); err != nil {
		return fmt.Errorf("put re-encrypted part: %w", err)
	}

	enc := &core.EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(result.BaseNonce, result.WrappedDEK),
		KeyID:         result.KeyID,
		PlaintextSize: int64(len(plaintext)),
	}
	if err := b.store.UpdatePartEncryption(ctx, mu.UploadID, part.PartNumber, int64(len(newCiphertext)), enc); err != nil {
		return fmt.Errorf("update part row: %w", err)
	}
	return nil
}

// decryptLegacyPart drains a part's ciphertext stream and returns the
// plaintext bytes. The part row carries its own wrapped DEK + key id;
// we let the encryptor unwrap and decrypt as if the part were a
// stand-alone object (which is what the legacy format actually is).
func decryptLegacyPart(ctx context.Context, enc *encryption.Encryptor, r io.Reader, part *core.MultipartPart) ([]byte, error) {
	if !part.Encrypted || len(part.EncryptionKey) == 0 {
		// A part recorded without encryption while the upload row is
		// flagged as legacy means the operator deployed the
		// orchestrator with encryption mid-upload. Skip rather than
		// rebuild because there is no DEK to migrate; the part stays
		// as-is and the assembled object will be plaintext.
		return io.ReadAll(r)
	}
	_, wrappedDEK, err := encryption.UnpackKeyData(part.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("unpack part key data: %w", err)
	}
	plain, err := enc.Decrypt(ctx, r, wrappedDEK, part.KeyID)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return io.ReadAll(plain)
}

// multipartPartKey mirrors the runtime helper of the same name so the
// worker resolves the same temp keys the proxy uses.
func multipartPartKey(uploadID string, partNumber int) string {
	return fmt.Sprintf("__multipart/%s/%d", uploadID, partNumber)
}

// multipartLockID derives the same advisory lock ID the runtime uses
// for CompleteMultipartUpload contention. The high-bit namespace keeps
// per-upload locks well above the small fixed service lock IDs.
func multipartLockID(uploadID string) int64 {
	const namespace int64 = 1 << 62
	h := fnv.New64a()
	_, _ = h.Write([]byte(uploadID))
	return namespace | int64(h.Sum64()&((1<<62)-1))
}

// -------------------------------------------------------------------------
// SCHEDULED EXECUTION
// -------------------------------------------------------------------------

// RunOnce blocks until the backlog is empty, returning the migration
// count. Intended for the cli/serve startup hook so the proxy refuses
// to accept traffic until every legacy upload has been migrated.
func (b *MultipartBackfill) RunOnce(ctx context.Context) (int, error) {
	migrated, err := b.Run(ctx)
	if err != nil {
		return migrated, err
	}
	if migrated > 0 {
		slog.InfoContext(ctx, "multipart_dek_backfill: completed",
			"migrated", migrated)
	}
	return migrated, nil
}

// RunPeriodic runs Run on the given interval until ctx is cancelled.
// Intended for the periodic re-run that picks up any rows the startup
// hook could not migrate (e.g., backend was unavailable at boot but
// recovered later).
func (b *MultipartBackfill) RunPeriodic(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := b.Run(ctx); err != nil {
				slog.WarnContext(ctx, "multipart_dek_backfill: periodic run failed",
					logfmt.Err(err))
			}
		}
	}
}
