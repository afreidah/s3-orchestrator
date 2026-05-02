// -------------------------------------------------------------------------------
// SQLite TxAdapter - Per-Engine Transactional Seam
//
// Author: Alex Freidah
//
// Implements core.TxAdapter against a *sql.Tx scoped to an open SQLite
// transaction. Engine-agnostic business logic in core/ runs through this
// adapter without touching the underlying database/sql calls. SQLite
// serializes writers, so AcquireKeyLock is a silent no-op and ClaimPending
// uses an existence probe rather than a row-level lock.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TX ADAPTER
// -------------------------------------------------------------------------

// sqliteTxAdapter implements core.TxAdapter over a *sql.Tx.
type sqliteTxAdapter struct {
	tx *sql.Tx
}

// AcquireKeyLock is a silent no-op on SQLite. The engine serializes
// writers, so the in-tx existence checks in ClaimPending and
// GetExistingCopiesForUpdate provide the equivalent guarantee that
// pg_advisory_xact_lock provides on Postgres.
func (a *sqliteTxAdapter) AcquireKeyLock(_ context.Context, _ string) error {
	return nil
}

// -------------------------------------------------------------------------
// PENDING TX OPERATIONS
// -------------------------------------------------------------------------

// ClaimPending reports whether a pending row exists for the given
// intent. Inside a writer-serialized tx, presence is the same guarantee
// Postgres gets from SELECT FOR UPDATE.
func (a *sqliteTxAdapter) ClaimPending(ctx context.Context, intentID string) (bool, error) {
	var probe string
	err := a.tx.QueryRowContext(ctx,
		`SELECT intent_id FROM pending_objects WHERE intent_id = ?`, intentID,
	).Scan(&probe)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim pending: %w", err)
	}
	return true, nil
}

// InsertPending records an in-flight PUT intent.
func (a *sqliteTxAdapter) InsertPending(ctx context.Context, p *core.PendingObject) error {
	encrypted := 0
	if p.Encrypted {
		encrypted = 1
	}
	if _, err := a.tx.ExecContext(ctx,
		`INSERT INTO pending_objects
		   (intent_id, object_key, backend_name, size_bytes,
		    encrypted, encryption_key, key_id, plaintext_size, content_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.IntentID, p.ObjectKey, p.BackendName, p.SizeBytes,
		encrypted, p.EncryptionKey,
		nullableString(p.KeyID), nullableInt64(p.PlaintextSize), nullableString(p.ContentHash),
	); err != nil {
		return fmt.Errorf("insert pending object: %w", err)
	}
	return nil
}

// DeletePending removes a pending intent.
func (a *sqliteTxAdapter) DeletePending(ctx context.Context, intentID string) error {
	if _, err := a.tx.ExecContext(ctx,
		`DELETE FROM pending_objects WHERE intent_id = ?`, intentID,
	); err != nil {
		return fmt.Errorf("delete pending object: %w", err)
	}
	return nil
}

// DeletePendingByBackend removes every pending intent for a backend.
func (a *sqliteTxAdapter) DeletePendingByBackend(ctx context.Context, backendName string) error {
	if _, err := a.tx.ExecContext(ctx,
		`DELETE FROM pending_objects WHERE backend_name = ?`, backendName,
	); err != nil {
		return fmt.Errorf("delete pending objects by backend: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// OBJECTS TX OPERATIONS
// -------------------------------------------------------------------------

// GetExistingCopiesForUpdate returns every copy of a key. SQLite's
// single-writer model means no row lock is needed inside a write tx.
func (a *sqliteTxAdapter) GetExistingCopiesForUpdate(ctx context.Context, objectKey string) ([]core.ExistingCopy, error) {
	rows, err := a.tx.QueryContext(ctx,
		`SELECT backend_name, size_bytes, created_at
		 FROM object_locations
		 WHERE object_key = ?`, objectKey)
	if err != nil {
		return nil, fmt.Errorf("query existing copies: %w", err)
	}
	defer rows.Close()

	var out []core.ExistingCopy
	for rows.Next() {
		var (
			ec        core.ExistingCopy
			createdAt string
		)
		if err := rows.Scan(&ec.BackendName, &ec.SizeBytes, &createdAt); err != nil {
			return nil, fmt.Errorf("scan existing copy: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			ec.CreatedAt = t
		}
		out = append(out, ec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate existing copies: %w", err)
	}
	return out, nil
}

// GetCopiesForKeysForUpdate returns every (key, backend, size) row
// matching any key in the supplied list. The query uses SQLite's
// json_each so the SQL stays static and the keys array is passed as
// a single JSON-encoded parameter rather than interpolated into the
// SQL string. FOR UPDATE is a silent no-op since SQLite serializes
// writers; the in-tx read provides the same exclusivity guarantee.
func (a *sqliteTxAdapter) GetCopiesForKeysForUpdate(ctx context.Context, keys []string) ([]core.KeyedExistingCopy, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return nil, fmt.Errorf("marshal keys: %w", err)
	}
	rows, err := a.tx.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes
		FROM object_locations
		WHERE object_key IN (SELECT value FROM json_each(?))`, string(keysJSON))
	if err != nil {
		return nil, fmt.Errorf("get copies for keys: %w", err)
	}
	defer rows.Close()
	var out []core.KeyedExistingCopy
	for rows.Next() {
		var ec core.KeyedExistingCopy
		if err := rows.Scan(&ec.ObjectKey, &ec.BackendName, &ec.SizeBytes); err != nil {
			return nil, fmt.Errorf("scan keyed copy: %w", err)
		}
		out = append(out, ec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate keyed copies: %w", err)
	}
	return out, nil
}

// DeleteObjectsByKeys bulk-deletes object_locations rows for every
// supplied key. Caller must have already locked the rows via
// GetCopiesForKeysForUpdate. Uses json_each so the SQL stays static
// and the keys array is passed as a JSON parameter.
func (a *sqliteTxAdapter) DeleteObjectsByKeys(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("marshal keys: %w", err)
	}
	if _, err := a.tx.ExecContext(ctx, `
		DELETE FROM object_locations
		WHERE object_key IN (SELECT value FROM json_each(?))`, string(keysJSON)); err != nil {
		return fmt.Errorf("delete objects by keys: %w", err)
	}
	return nil
}

// InsertObjectLocation writes a new object_locations row carrying the
// encryption and integrity metadata on loc.
func (a *sqliteTxAdapter) InsertObjectLocation(ctx context.Context, loc *core.ObjectLocation) error {
	encrypted := 0
	if loc.Encrypted {
		encrypted = 1
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := a.tx.ExecContext(ctx,
		`INSERT INTO object_locations
		   (object_key, backend_name, size_bytes, encrypted, encryption_key,
		    key_id, plaintext_size, content_hash, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		loc.ObjectKey, loc.BackendName, loc.SizeBytes, encrypted,
		loc.EncryptionKey,
		nullableString(loc.KeyID), nullableInt64(loc.PlaintextSize), nullableString(loc.ContentHash),
		now,
	); err != nil {
		return fmt.Errorf("insert object location: %w", err)
	}
	return nil
}

// DeleteObjectCopies removes every object_locations row for the key.
func (a *sqliteTxAdapter) DeleteObjectCopies(ctx context.Context, objectKey string) error {
	if _, err := a.tx.ExecContext(ctx,
		`DELETE FROM object_locations WHERE object_key = ?`, objectKey,
	); err != nil {
		return fmt.Errorf("delete object copies: %w", err)
	}
	return nil
}

// CheckObjectExistsOnBackend reports whether (key, backend) is in
// object_locations.
func (a *sqliteTxAdapter) CheckObjectExistsOnBackend(ctx context.Context, objectKey, backend string) (bool, error) {
	var probe int
	err := a.tx.QueryRowContext(ctx,
		`SELECT 1 FROM object_locations WHERE object_key = ? AND backend_name = ?`,
		objectKey, backend,
	).Scan(&probe)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check object on backend: %w", err)
	}
	return true, nil
}

// LockObjectOnBackend reads the (key, backend) row's full payload
// inside the writer-serialized tx. (nil, false, nil) means the row is
// gone - benign race.
func (a *sqliteTxAdapter) LockObjectOnBackend(ctx context.Context, objectKey, backend string) (*core.ObjectLocation, bool, error) {
	var (
		size          int64
		encrypted     int
		encryptionKey []byte
		keyID         sql.NullString
		plaintextSize sql.NullInt64
		contentHash   sql.NullString
	)
	err := a.tx.QueryRowContext(ctx,
		`SELECT size_bytes, encrypted, encryption_key,
		        key_id, plaintext_size, content_hash
		 FROM object_locations
		 WHERE object_key = ? AND backend_name = ?`,
		objectKey, backend,
	).Scan(&size, &encrypted, &encryptionKey, &keyID, &plaintextSize, &contentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock object on backend: %w", err)
	}
	loc := &core.ObjectLocation{
		ObjectKey:     objectKey,
		BackendName:   backend,
		SizeBytes:     size,
		Encrypted:     encrypted != 0,
		EncryptionKey: encryptionKey,
	}
	if keyID.Valid {
		loc.KeyID = keyID.String
	}
	if plaintextSize.Valid {
		loc.PlaintextSize = plaintextSize.Int64
	}
	if contentHash.Valid {
		loc.ContentHash = contentHash.String
	}
	return loc, true, nil
}

// DeleteObjectFromBackend removes the single (key, backend) row.
func (a *sqliteTxAdapter) DeleteObjectFromBackend(ctx context.Context, objectKey, backend string) error {
	if _, err := a.tx.ExecContext(ctx,
		`DELETE FROM object_locations WHERE object_key = ? AND backend_name = ?`,
		objectKey, backend,
	); err != nil {
		return fmt.Errorf("delete object from backend: %w", err)
	}
	return nil
}

// InsertObjectLocationIfNotExists inserts a row only if one does not
// already exist for (key, backend). Returns true when a row was newly
// inserted.
func (a *sqliteTxAdapter) InsertObjectLocationIfNotExists(ctx context.Context, loc *core.ObjectLocation) (bool, error) {
	exists, err := a.CheckObjectExistsOnBackend(ctx, loc.ObjectKey, loc.BackendName)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if err := a.InsertObjectLocation(ctx, loc); err != nil {
		return false, err
	}
	return true, nil
}

// InsertReplicaConditional inserts a replica row only if the source
// copy still exists and the target does not already have a copy.
func (a *sqliteTxAdapter) InsertReplicaConditional(ctx context.Context, objectKey, targetBackend, sourceBackend string) (bool, error) {
	srcLoc, ok, err := a.LockObjectOnBackend(ctx, objectKey, sourceBackend)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	targetExists, err := a.CheckObjectExistsOnBackend(ctx, objectKey, targetBackend)
	if err != nil {
		return false, err
	}
	if targetExists {
		return false, nil
	}
	dest := &core.ObjectLocation{
		ObjectKey:     objectKey,
		BackendName:   targetBackend,
		SizeBytes:     srcLoc.SizeBytes,
		Encrypted:     srcLoc.Encrypted,
		EncryptionKey: srcLoc.EncryptionKey,
		KeyID:         srcLoc.KeyID,
		PlaintextSize: srcLoc.PlaintextSize,
		ContentHash:   srcLoc.ContentHash,
	}
	if err := a.InsertObjectLocation(ctx, dest); err != nil {
		return false, err
	}
	return true, nil
}

// -------------------------------------------------------------------------
// CLEANUP TX OPERATIONS
// -------------------------------------------------------------------------

// SumAndDeleteCleanupQueueRows deletes every cleanup_queue row for
// (key, backend) and returns the count and total size of those rows.
func (a *sqliteTxAdapter) SumAndDeleteCleanupQueueRows(ctx context.Context, objectKey, backend string) (int64, int64, error) {
	var totalBytes sql.NullInt64
	var rowCount int64
	if err := a.tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes), 0), COUNT(*)
		 FROM cleanup_queue
		 WHERE object_key = ? AND backend_name = ?`,
		objectKey, backend,
	).Scan(&totalBytes, &rowCount); err != nil {
		return 0, 0, fmt.Errorf("sum cleanup queue size: %w", err)
	}
	if rowCount == 0 {
		return 0, 0, nil
	}
	if _, err := a.tx.ExecContext(ctx,
		`DELETE FROM cleanup_queue WHERE object_key = ? AND backend_name = ?`,
		objectKey, backend,
	); err != nil {
		return 0, 0, fmt.Errorf("delete cleanup queue rows: %w", err)
	}
	return rowCount, totalBytes.Int64, nil
}

// -------------------------------------------------------------------------
// QUOTA TX OPERATIONS
// -------------------------------------------------------------------------

// IncrementBackendQuota credits delta bytes to backendName. Returns
// core.ErrNoSpaceAvailable when the guarded UPDATE touches zero rows
// (quota ceiling would be exceeded).
func (a *sqliteTxAdapter) IncrementBackendQuota(ctx context.Context, backendName string, delta int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := a.tx.ExecContext(ctx, `
		UPDATE backend_quotas
		SET bytes_used = bytes_used + ?, updated_at = ?
		WHERE backend_name = ?
		  AND (bytes_limit = 0 OR bytes_used + ? <= bytes_limit)`,
		delta, now, backendName, delta)
	if err != nil {
		return fmt.Errorf("increment quota: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check quota update: %w", err)
	}
	if n == 0 {
		return core.ErrNoSpaceAvailable
	}
	return nil
}

// DecrementBackendQuota debits delta bytes from backendName.
func (a *sqliteTxAdapter) DecrementBackendQuota(ctx context.Context, backendName string, delta int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := a.tx.ExecContext(ctx, `
		UPDATE backend_quotas
		SET bytes_used = MAX(0, bytes_used - ?), updated_at = ?
		WHERE backend_name = ?`, delta, now, backendName); err != nil {
		return fmt.Errorf("decrement quota for %s: %w", backendName, err)
	}
	return nil
}

// DecrementOrphanBytes debits delta bytes from the backend's
// orphan_bytes counter (clamped at zero).
func (a *sqliteTxAdapter) DecrementOrphanBytes(ctx context.Context, backendName string, delta int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := a.tx.ExecContext(ctx, `
		UPDATE backend_quotas
		SET orphan_bytes = MAX(0, orphan_bytes - ?), updated_at = ?
		WHERE backend_name = ?`, delta, now, backendName); err != nil {
		return fmt.Errorf("decrement orphan bytes: %w", err)
	}
	return nil
}

// Compile-time check that *sqliteTxAdapter satisfies core.TxAdapter.
var _ core.TxAdapter = (*sqliteTxAdapter)(nil)
