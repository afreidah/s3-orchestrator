// -------------------------------------------------------------------------------
// Postgres TxAdapter - Per-Engine Transactional Seam
//
// Author: Alex Freidah
//
// Implements core.TxAdapter against a sqlc-generated Queries scoped to an
// open pgx transaction. Adapter methods translate between core domain types
// and the sqlc-generated row structs so engine-agnostic business logic in
// core/ never touches pgx-specific values.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// -------------------------------------------------------------------------
// TX ADAPTER
// -------------------------------------------------------------------------

// pgTxAdapter implements core.TxAdapter over a sqlc Queries scoped to a
// transaction.
type pgTxAdapter struct {
	q *db.Queries
}

// AcquireKeyLock takes a transaction-scoped advisory lock keyed by
// objectKey. Postgres uses pg_advisory_xact_lock(hashtext(...)) under
// the hood via the sqlc query.
func (a *pgTxAdapter) AcquireKeyLock(ctx context.Context, objectKey string) error {
	if err := a.q.LockObjectKeyForWrite(ctx, objectKey); err != nil {
		return fmt.Errorf("acquire object key lock: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// PENDING TX OPERATIONS
// -------------------------------------------------------------------------

// ClaimPending returns true if the pending row exists and was locked
// FOR UPDATE; false if it has already been resolved.
func (a *pgTxAdapter) ClaimPending(ctx context.Context, intentID string) (bool, error) {
	if _, err := a.q.LockPendingForUpdate(ctx, intentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("lock pending row: %w", err)
	}
	return true, nil
}

// DeletePending removes a pending intent.
func (a *pgTxAdapter) DeletePending(ctx context.Context, intentID string) error {
	if err := a.q.DeletePendingObject(ctx, intentID); err != nil {
		return fmt.Errorf("delete pending object: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// OBJECTS TX OPERATIONS
// -------------------------------------------------------------------------

// GetExistingCopiesForUpdate returns every copy of a key, taking row
// locks suitable for an in-flight overwrite or promotion.
func (a *pgTxAdapter) GetExistingCopiesForUpdate(ctx context.Context, objectKey string) ([]core.ExistingCopy, error) {
	rows, err := a.q.GetExistingCopiesForUpdate(ctx, objectKey)
	if err != nil {
		return nil, fmt.Errorf("query existing copies: %w", err)
	}
	return existingCopiesFromRows(rows), nil
}

// InsertObjectLocation inserts an object_locations row carrying any
// encryption and integrity metadata on the supplied location.
func (a *pgTxAdapter) InsertObjectLocation(ctx context.Context, loc *core.ObjectLocation) error {
	if err := a.q.InsertObjectLocation(ctx, objectInsertParams(loc)); err != nil {
		return fmt.Errorf("insert object location: %w", err)
	}
	return nil
}

// DeleteObjectCopies removes every object_locations row for the key.
func (a *pgTxAdapter) DeleteObjectCopies(ctx context.Context, objectKey string) error {
	if err := a.q.DeleteObjectCopies(ctx, objectKey); err != nil {
		return fmt.Errorf("delete object copies: %w", err)
	}
	return nil
}

// CheckObjectExistsOnBackend reports whether (objectKey, backend) is
// present in object_locations.
func (a *pgTxAdapter) CheckObjectExistsOnBackend(ctx context.Context, objectKey, backend string) (bool, error) {
	exists, err := a.q.CheckObjectExistsOnBackend(ctx, db.CheckObjectExistsOnBackendParams{
		ObjectKey:   objectKey,
		BackendName: backend,
	})
	if err != nil {
		return false, fmt.Errorf("check object on backend: %w", err)
	}
	return exists, nil
}

// LockObjectOnBackend takes a FOR UPDATE lock on the (key, backend)
// row and returns its full payload. (nil, false, nil) means the row
// is gone - benign race.
func (a *pgTxAdapter) LockObjectOnBackend(ctx context.Context, objectKey, backend string) (*core.ObjectLocation, bool, error) {
	row, err := a.q.LockObjectOnBackend(ctx, db.LockObjectOnBackendParams{
		ObjectKey:   objectKey,
		BackendName: backend,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock object on backend: %w", err)
	}
	loc := &core.ObjectLocation{
		ObjectKey:                objectKey,
		BackendName:              backend,
		SizeBytes:                row.SizeBytes,
		Encrypted:                row.Encrypted,
		EncryptionKey:            row.EncryptionKey,
		KeyID:                    derefStr(row.KeyID),
		PlaintextSize:            derefInt64(row.PlaintextSize),
		ContentHash:              derefStr(row.ContentHash),
		CompressionAlgorithm:     derefStr(row.CompressionAlgorithm),
		CompressionLevel:         derefStr(row.CompressionLevel),
		CompressionFormatVersion: int(derefInt16(row.CompressionFormatVersion)),
		LogicalSize:              derefInt64(row.LogicalSize),
		CompressionProbeSize:     derefInt64(row.CompressionProbeSize),
		CompressionProbeLevel:    derefStr(row.CompressionProbeLevel),
	}
	// Carried so a move keeps the object's identity: the bytes are the same
	// bytes, so the client-facing answer does not change with their address.
	loc.Identity, _ = core.IdentityFromColumns(derefStr(row.Etag), derefStr(row.ContentType), row.UserMetadata)
	return loc, true, nil
}

// DeleteObjectFromBackend removes the single (objectKey, backend)
// object_locations row.
func (a *pgTxAdapter) DeleteObjectFromBackend(ctx context.Context, objectKey, backend string) error {
	if err := a.q.DeleteObjectFromBackend(ctx, db.DeleteObjectFromBackendParams{
		ObjectKey:   objectKey,
		BackendName: backend,
	}); err != nil {
		return fmt.Errorf("delete object from backend: %w", err)
	}
	return nil
}

// RecordCompressionProbe stores what the encoder measured for a copy it
// declined to store compressed.
func (a *pgTxAdapter) RecordCompressionProbe(ctx context.Context, probe *core.CompressionProbe) error {
	if err := a.q.RecordCompressionProbe(ctx, db.RecordCompressionProbeParams{
		ObjectKey:             probe.ObjectKey,
		BackendName:           probe.BackendName,
		CompressionProbeSize:  int64Ptr(probe.Size),
		CompressionProbeLevel: strPtr(probe.Level),
	}); err != nil {
		return fmt.Errorf("record compression probe: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// OBJECT TAGS
// -------------------------------------------------------------------------

// InsertObjectTag adds one tag row for an object.
func (a *pgTxAdapter) InsertObjectTag(ctx context.Context, objectKey, tagKey, tagValue string) error {
	if err := a.q.InsertObjectTag(ctx, db.InsertObjectTagParams{
		ObjectKey: objectKey,
		TagKey:    tagKey,
		TagValue:  tagValue,
	}); err != nil {
		return fmt.Errorf("insert object tag: %w", err)
	}
	return nil
}

// DeleteObjectTags removes every tag row for one object key.
func (a *pgTxAdapter) DeleteObjectTags(ctx context.Context, objectKey string) error {
	if err := a.q.DeleteObjectTags(ctx, objectKey); err != nil {
		return fmt.Errorf("delete object tags: %w", err)
	}
	return nil
}

// DeleteObjectTagsForKeys removes every tag row for any of the given keys.
func (a *pgTxAdapter) DeleteObjectTagsForKeys(ctx context.Context, objectKeys []string) error {
	if err := a.q.DeleteObjectTagsForKeys(ctx, objectKeys); err != nil {
		return fmt.Errorf("delete object tags for keys: %w", err)
	}
	return nil
}

// InsertObjectLocationIfNotExists inserts a row only when one does not
// already exist for (key, backend). Returns true when the row was newly
// inserted.
func (a *pgTxAdapter) InsertObjectLocationIfNotExists(ctx context.Context, loc *core.ObjectLocation) (bool, error) {
	inserted, err := a.q.InsertObjectLocationIfNotExists(ctx, objectInsertIfNotExistsParams(loc))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert object location if not exists: %w", err)
	}
	return inserted, nil
}

// GetCopiesForKeysForUpdate returns every (key, backend, size) row
// matching any key in the supplied list, locked FOR UPDATE.
func (a *pgTxAdapter) GetCopiesForKeysForUpdate(ctx context.Context, keys []string) ([]core.KeyedExistingCopy, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := a.q.GetCopiesForKeysForUpdate(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("get copies for keys: %w", err)
	}
	return mapSlice(rows, keyedExistingCopyFromRow), nil
}

// keyedExistingCopyFromRow converts a sqlc GetCopiesForKeysForUpdate row
// to core.KeyedExistingCopy.
func keyedExistingCopyFromRow(r *db.GetCopiesForKeysForUpdateRow) core.KeyedExistingCopy {
	return core.KeyedExistingCopy{
		ObjectKey:   r.ObjectKey,
		BackendName: r.BackendName,
		SizeBytes:   r.SizeBytes,
	}
}

// DeleteObjectsByKeys bulk-deletes object_locations rows for every
// supplied key.
func (a *pgTxAdapter) DeleteObjectsByKeys(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := a.q.DeleteObjectsByKeys(ctx, keys); err != nil {
		return fmt.Errorf("delete objects by keys: %w", err)
	}
	return nil
}

// InsertReplicaConditional inserts a replica row only if the source
// copy still exists. Returns the inserted size_bytes (read from the
// source row in the same statement) on success, or (0, false, nil)
// when the source copy is gone - a benign race the caller treats as
// nothing-to-do.
func (a *pgTxAdapter) InsertReplicaConditional(ctx context.Context, objectKey, targetBackend, sourceBackend string) (int64, bool, error) {
	size, err := a.q.InsertReplicaConditional(ctx, db.InsertReplicaConditionalParams{
		ObjectKey:     objectKey,
		TargetBackend: targetBackend,
		SourceBackend: sourceBackend,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("insert replica conditional: %w", err)
	}
	return size, true, nil
}

// -------------------------------------------------------------------------
// CLEANUP TX OPERATIONS
// -------------------------------------------------------------------------

// SumAndDeleteCleanupQueueRows deletes every cleanup_queue row for the
// (objectKey, backend) pair and returns the count and total size of the
// rows that existed prior to the delete.
func (a *pgTxAdapter) SumAndDeleteCleanupQueueRows(ctx context.Context, objectKey, backend string) (int64, int64, error) {
	sum, err := a.q.SumCleanupQueueSizeByKey(ctx, db.SumCleanupQueueSizeByKeyParams{
		ObjectKey:   objectKey,
		BackendName: backend,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("sum cleanup queue size: %w", err)
	}
	if sum.RowCount == 0 {
		return 0, 0, nil
	}
	if _, err := a.q.DeleteCleanupQueueByKey(ctx, db.DeleteCleanupQueueByKeyParams{
		ObjectKey:   objectKey,
		BackendName: backend,
	}); err != nil {
		return 0, 0, fmt.Errorf("delete cleanup queue rows: %w", err)
	}
	return sum.RowCount, sum.TotalBytes, nil
}

// GetCleanupQueueRow returns the full payload of a single cleanup_queue
// row by id. Inside MoveCleanupToDLQ this read carries every column the
// DLQ insert needs in one round trip.
func (a *pgTxAdapter) GetCleanupQueueRow(ctx context.Context, id int64) (core.CleanupQueueRow, error) {
	row, err := a.q.GetCleanupQueueRow(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.CleanupQueueRow{}, core.ErrCleanupItemNotFound
		}
		return core.CleanupQueueRow{}, fmt.Errorf("get cleanup queue row: %w", err)
	}
	out := core.CleanupQueueRow{
		ID:          row.ID,
		BackendName: row.BackendName,
		ObjectKey:   row.ObjectKey,
		Reason:      row.Reason,
		SizeBytes:   row.SizeBytes,
		Attempts:    row.Attempts,
		LastError:   derefStr(row.LastError),
	}
	if row.CreatedAt.Valid {
		out.CreatedAt = row.CreatedAt.Time
	}
	return out, nil
}

// InsertCleanupDLQ inserts row into cleanup_dlq. Bytes are not
// reconciled here because the underlying object is still on the backend;
// orphan_bytes accounting stays intentionally untouched on the move.
func (a *pgTxAdapter) InsertCleanupDLQ(ctx context.Context, row *core.CleanupQueueRow) error {
	lastErr := strPtr(row.LastError)
	firstEnqueuedAt := pgtype.Timestamptz{Valid: false}
	if !row.CreatedAt.IsZero() {
		firstEnqueuedAt = pgtype.Timestamptz{Time: row.CreatedAt, Valid: true}
	}
	if err := a.q.InsertCleanupDLQ(ctx, db.InsertCleanupDLQParams{
		OriginalID:      row.ID,
		BackendName:     row.BackendName,
		ObjectKey:       row.ObjectKey,
		Reason:          row.Reason,
		SizeBytes:       row.SizeBytes,
		Attempts:        row.Attempts,
		FirstEnqueuedAt: firstEnqueuedAt,
		LastError:       lastErr,
	}); err != nil {
		return fmt.Errorf("insert cleanup_dlq: %w", err)
	}
	return nil
}

// DeleteCleanupItem removes the cleanup_queue row by id. Used inside
// MoveCleanupToDLQ so the queue->DLQ move is atomic with the insert
// above.
func (a *pgTxAdapter) DeleteCleanupItem(ctx context.Context, id int64) error {
	if err := a.q.DeleteCleanupItem(ctx, id); err != nil {
		return fmt.Errorf("delete cleanup_queue row: %w", err)
	}
	return nil
}

// HasPendingCleanup reports whether a delete for (objectKey, backend) is still
// outstanding in either the retry queue or the dead-letter table.
func (a *pgTxAdapter) HasPendingCleanup(ctx context.Context, objectKey, backend string) (bool, error) {
	pending, err := a.q.HasPendingCleanup(ctx, db.HasPendingCleanupParams{
		ObjectKey:   objectKey,
		BackendName: backend,
	})
	if err != nil {
		return false, fmt.Errorf("check pending cleanup: %w", err)
	}
	return pending, nil
}

// -------------------------------------------------------------------------
// QUOTA TX OPERATIONS
// -------------------------------------------------------------------------

// AdjustQuotaStripe applies a signed delta to one of a backend's byte-counter
// stripes, materializing the row on first use.
func (a *pgTxAdapter) AdjustQuotaStripe(ctx context.Context, backendName string, stripe int16, delta int64) error {
	if err := a.q.AdjustQuotaStripe(ctx, db.AdjustQuotaStripeParams{
		BackendName: backendName,
		StripeID:    stripe,
		Delta:       delta,
	}); err != nil {
		return fmt.Errorf("adjust quota stripe for %s: %w", backendName, err)
	}
	return nil
}

// DecrementOrphanBytes debits delta bytes from the backend's
// orphan_bytes counter (clamped at zero by the SQL).
func (a *pgTxAdapter) DecrementOrphanBytes(ctx context.Context, backendName string, delta int64) error {
	if err := a.q.DecrementOrphanBytes(ctx, db.DecrementOrphanBytesParams{
		Amount:      delta,
		BackendName: backendName,
	}); err != nil {
		return fmt.Errorf("decrement orphan bytes: %w", err)
	}
	return nil
}

// AllBackendBytesUsed returns the current bytes_used for every
// backend_quotas row, keyed by backend name.
func (a *pgTxAdapter) AllBackendBytesUsed(ctx context.Context) (map[string]int64, error) {
	rows, err := a.q.GetAllQuotaStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("read all quota stats: %w", err)
	}
	return totalsByBackend(rows, func(r db.GetAllQuotaStatsRow) (string, int64) {
		return r.BackendName, r.BytesUsed
	}), nil
}

// totalsByBackend folds a per-backend result set into the map its caller
// returns, so the two callers state only which columns carry the name and the
// total.
func totalsByBackend[T any](rows []T, split func(T) (string, int64)) map[string]int64 {
	out := make(map[string]int64, len(rows))
	for i := range rows {
		name, total := split(rows[i])
		out[name] = total
	}
	return out
}

// SumObjectSizesByBackend returns SUM(size_bytes) per backend from the
// object_locations ledger, keyed by backend name.
func (a *pgTxAdapter) SumObjectSizesByBackend(ctx context.Context) (map[string]int64, error) {
	rows, err := a.q.SumObjectSizesByBackend(ctx)
	if err != nil {
		return nil, fmt.Errorf("sum object sizes by backend: %w", err)
	}
	return totalsByBackend(rows, func(r db.SumObjectSizesByBackendRow) (string, int64) {
		return r.BackendName, r.TotalBytes
	}), nil
}

// SetBackendBytesUsed overwrites bytes_used with the authoritative value.
func (a *pgTxAdapter) SetBackendBytesUsed(ctx context.Context, backendName string, value int64) error {
	if err := a.q.SetBackendBytesUsed(ctx, db.SetBackendBytesUsedParams{
		BytesUsed:   value,
		BackendName: backendName,
	}); err != nil {
		return fmt.Errorf("set backend bytes_used: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// STORED-FORM REWRITES
// -------------------------------------------------------------------------

// UpdateCompressedForm records the stored form a recompression pass left on
// one copy.
func (a *pgTxAdapter) UpdateCompressedForm(ctx context.Context, u *core.CompressedUpdate) error {
	if err := a.q.MarkObjectCompressed(ctx, db.MarkObjectCompressedParams{
		ObjectKey:                u.ObjectKey,
		BackendName:              u.BackendName,
		CompressionAlgorithm:     strPtr(u.Algorithm),
		CompressionLevel:         strPtr(u.Level),
		CompressionFormatVersion: int16Ptr(u.FormatVersion),
		LogicalSize:              int64Ptr(u.LogicalSize),
		SizeBytes:                u.SizeBytes,
		PlaintextSize:            int64Ptr(u.PlaintextSize),
		EncryptionKey:            u.EncryptionKey,
		KeyID:                    strPtr(u.KeyID),
	}); err != nil {
		return fmt.Errorf("update compressed form: %w", err)
	}
	return nil
}

// MarkCopyEncrypted records the envelope columns for a copy encrypted in place.
func (a *pgTxAdapter) MarkCopyEncrypted(ctx context.Context, u *core.EncryptedUpdate) error {
	if err := a.q.MarkObjectEncrypted(ctx, db.MarkObjectEncryptedParams{
		ObjectKey:     u.ObjectKey,
		BackendName:   u.BackendName,
		EncryptionKey: u.EncryptionKey,
		KeyID:         &u.KeyID,
		PlaintextSize: &u.PlaintextSize,
		SizeBytes:     u.CiphertextSize,
	}); err != nil {
		return fmt.Errorf("mark copy encrypted: %w", err)
	}
	return nil
}

// MarkCopyDecrypted clears the envelope columns for a copy decrypted in place.
func (a *pgTxAdapter) MarkCopyDecrypted(ctx context.Context, objectKey, backendName string, plaintextSize int64) error {
	if err := a.q.MarkObjectDecrypted(ctx, db.MarkObjectDecryptedParams{
		ObjectKey:   objectKey,
		BackendName: backendName,
		SizeBytes:   plaintextSize,
	}); err != nil {
		return fmt.Errorf("mark copy decrypted: %w", err)
	}
	return nil
}

// GetCopySizeBytes reads the size a copy currently reports.
func (a *pgTxAdapter) GetCopySizeBytes(ctx context.Context, objectKey, backendName string) (int64, error) {
	size, err := a.q.GetObjectSizeBytes(ctx, db.GetObjectSizeBytesParams{
		ObjectKey:   objectKey,
		BackendName: backendName,
	})
	if err != nil {
		return 0, fmt.Errorf("get copy size_bytes: %w", err)
	}
	return size, nil
}

// Compile-time check that *pgTxAdapter satisfies core.TxAdapter.
var _ core.TxAdapter = (*pgTxAdapter)(nil)
