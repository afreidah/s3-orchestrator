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

// InsertPending inserts a new pending intent.
func (a *pgTxAdapter) InsertPending(ctx context.Context, p *core.PendingObject) error {
	if err := a.q.InsertPendingObject(ctx, pendingInsertParams(p)); err != nil {
		return fmt.Errorf("insert pending object: %w", err)
	}
	return nil
}

// DeletePending removes a pending intent.
func (a *pgTxAdapter) DeletePending(ctx context.Context, intentID string) error {
	if err := a.q.DeletePendingObject(ctx, intentID); err != nil {
		return fmt.Errorf("delete pending object: %w", err)
	}
	return nil
}

// DeletePendingByBackend removes every pending intent for a backend.
// Used during backend drain finalization so abandoned intents do not
// outlive their backend's row in backend_quotas (FK cascade safety).
func (a *pgTxAdapter) DeletePendingByBackend(ctx context.Context, backendName string) error {
	if err := a.q.DeletePendingObjectsByBackend(ctx, backendName); err != nil {
		return fmt.Errorf("delete pending objects by backend: %w", err)
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
		ObjectKey:     objectKey,
		BackendName:   backend,
		SizeBytes:     row.SizeBytes,
		Encrypted:     row.Encrypted,
		EncryptionKey: row.EncryptionKey,
		KeyID:         derefStr(row.KeyID),
		PlaintextSize: derefInt64(row.PlaintextSize),
		ContentHash:   derefStr(row.ContentHash),
	}
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

// InsertObjectLocationIfNotExists inserts a row only when one does not
// already exist for (key, backend). Returns true when the row was newly
// inserted.
func (a *pgTxAdapter) InsertObjectLocationIfNotExists(ctx context.Context, loc *core.ObjectLocation) (bool, error) {
	inserted, err := a.q.InsertObjectLocationIfNotExists(ctx, db.InsertObjectLocationIfNotExistsParams{
		ObjectKey:   loc.ObjectKey,
		BackendName: loc.BackendName,
		SizeBytes:   loc.SizeBytes,
	})
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
	out := make([]core.KeyedExistingCopy, len(rows))
	for i, r := range rows {
		out[i] = core.KeyedExistingCopy{
			ObjectKey:   r.ObjectKey,
			BackendName: r.BackendName,
			SizeBytes:   r.SizeBytes,
		}
	}
	return out, nil
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
		BackendName:   targetBackend,
		BackendName_2: sourceBackend,
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

// -------------------------------------------------------------------------
// QUOTA TX OPERATIONS
// -------------------------------------------------------------------------

// IncrementBackendQuota credits delta bytes to backendName. Returns
// core.ErrNoSpaceAvailable when the quota row reports zero rows updated
// (insufficient quota).
func (a *pgTxAdapter) IncrementBackendQuota(ctx context.Context, backendName string, delta int64) error {
	n, err := a.q.IncrementQuota(ctx, db.IncrementQuotaParams{
		Amount:      delta,
		BackendName: backendName,
	})
	if err != nil {
		return fmt.Errorf("increment quota: %w", err)
	}
	if n == 0 {
		return core.ErrNoSpaceAvailable
	}
	return nil
}

// DecrementBackendQuota debits delta bytes from backendName.
func (a *pgTxAdapter) DecrementBackendQuota(ctx context.Context, backendName string, delta int64) error {
	if err := a.q.DecrementQuota(ctx, db.DecrementQuotaParams{
		Amount:      delta,
		BackendName: backendName,
	}); err != nil {
		return fmt.Errorf("decrement quota for %s: %w", backendName, err)
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

// Compile-time check that *pgTxAdapter satisfies core.TxAdapter.
var _ core.TxAdapter = (*pgTxAdapter)(nil)
