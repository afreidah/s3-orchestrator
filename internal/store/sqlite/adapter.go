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

// DeletePending removes a pending intent.
func (a *sqliteTxAdapter) DeletePending(ctx context.Context, intentID string) error {
	if _, err := a.tx.ExecContext(ctx,
		`DELETE FROM pending_objects WHERE intent_id = ?`, intentID,
	); err != nil {
		return fmt.Errorf("delete pending object: %w", err)
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
		`SELECT backend_name, size_bytes, created_at, encrypted,
		        (encryption_key IS NOT NULL AND length(encryption_key) > 0)
		 FROM object_locations
		 WHERE object_key = ?`, objectKey)
	if err != nil {
		return nil, fmt.Errorf("query existing copies: %w", err)
	}
	return collectRows(rows, "existing copies", func(rows *sql.Rows) (core.ExistingCopy, error) {
		var (
			ec        core.ExistingCopy
			createdAt string
			encrypted int
			hasDEK    int
		)
		if err := rows.Scan(&ec.BackendName, &ec.SizeBytes, &createdAt, &encrypted, &hasDEK); err != nil {
			return core.ExistingCopy{}, fmt.Errorf("scan existing copy: %w", err)
		}
		ec.Encrypted = encrypted != 0
		ec.HasDEK = hasDEK != 0
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			ec.CreatedAt = t
		}
		return ec, nil
	})
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
	return collectRows(rows, "keyed copies", func(rows *sql.Rows) (core.KeyedExistingCopy, error) {
		var ec core.KeyedExistingCopy
		if err := rows.Scan(&ec.ObjectKey, &ec.BackendName, &ec.SizeBytes); err != nil {
			return core.KeyedExistingCopy{}, fmt.Errorf("scan keyed copy: %w", err)
		}
		return ec, nil
	})
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
	// An explicit time is the object's write time, carried from the source copy
	// on a replicate or reported by the backend on an import, and the row has to
	// keep it: stamping every copy separately is what makes an unmodified object
	// report a different Last-Modified depending on which one answered. A fresh
	// write leaves it zero and gets stamped here.
	created := now()
	if !loc.CreatedAt.IsZero() {
		created = formatTime(loc.CreatedAt)
	}
	if _, err := a.tx.ExecContext(ctx,
		`INSERT INTO object_locations
		   (object_key, backend_name, size_bytes, encrypted, encryption_key,
		    key_id, plaintext_size, content_hash,
		    compression_algorithm, compression_level, compression_format_version, logical_size,
		    etag, content_type, user_metadata,
		    managed, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		loc.ObjectKey, loc.BackendName, loc.SizeBytes, boolToInt(loc.Encrypted),
		loc.EncryptionKey,
		nullableString(loc.KeyID), nullableInt64(loc.PlaintextSize), nullableString(loc.ContentHash),
		nullableString(loc.CompressionAlgorithm), nullableString(loc.CompressionLevel),
		nullableInt64(int64(loc.CompressionFormatVersion)), nullableInt64(loc.LogicalSize),
		identityETag(loc.Identity), identityContentType(loc.Identity), identityMetadataJSON(loc.Identity),
		boolToInt(!loc.Unmanaged),
		created,
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
		compAlgorithm sql.NullString
		compLevel     sql.NullString
		compVersion   sql.NullInt64
		logicalSize   sql.NullInt64
		probeSize     sql.NullInt64
		probeLevel    sql.NullString
		createdAt     sql.NullString
	)
	err := a.tx.QueryRowContext(ctx,
		`SELECT size_bytes, encrypted, encryption_key,
		        key_id, plaintext_size, content_hash,
		        compression_algorithm, compression_level, compression_format_version, logical_size,
		        compression_probe_size, compression_probe_level, created_at
		 FROM object_locations
		 WHERE object_key = ? AND backend_name = ?`,
		objectKey, backend,
	).Scan(&size, &encrypted, &encryptionKey, &keyID, &plaintextSize, &contentHash,
		&compAlgorithm, &compLevel, &compVersion, &logicalSize, &probeSize, &probeLevel, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock object on backend: %w", err)
	}
	loc := &core.ObjectLocation{
		ObjectKey:                objectKey,
		BackendName:              backend,
		SizeBytes:                size,
		Encrypted:                encrypted != 0,
		EncryptionKey:            encryptionKey,
		KeyID:                    nullStringValue(keyID),
		PlaintextSize:            nullInt64Value(plaintextSize),
		ContentHash:              nullStringValue(contentHash),
		CompressionAlgorithm:     nullStringValue(compAlgorithm),
		CompressionLevel:         nullStringValue(compLevel),
		CompressionFormatVersion: int(nullInt64Value(compVersion)),
		LogicalSize:              nullInt64Value(logicalSize),
		CompressionProbeSize:     nullInt64Value(probeSize),
		CompressionProbeLevel:    nullStringValue(probeLevel),
	}
	// Carried so a replica built from this row inherits the object's write time
	// rather than being stamped when the copy was made. An unparseable value
	// leaves it zero, which the insert reads as "stamp your own".
	if t, err := time.Parse(time.RFC3339Nano, nullStringValue(createdAt)); err == nil {
		loc.CreatedAt = t
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

// RecordCompressionProbe stores what the encoder measured for a copy it
// declined to store compressed.
func (a *sqliteTxAdapter) RecordCompressionProbe(ctx context.Context, probe *core.CompressionProbe) error {
	if _, err := a.tx.ExecContext(ctx,
		`UPDATE object_locations
		 SET compression_probe_size = ?, compression_probe_level = ?
		 WHERE object_key = ? AND backend_name = ?`,
		probe.Size, probe.Level, probe.ObjectKey, probe.BackendName,
	); err != nil {
		return fmt.Errorf("record compression probe: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// OBJECT TAGS
// -------------------------------------------------------------------------

// InsertObjectTag adds one tag row for an object.
func (a *sqliteTxAdapter) InsertObjectTag(ctx context.Context, objectKey, tagKey, tagValue string) error {
	if _, err := a.tx.ExecContext(ctx,
		`INSERT INTO object_tags (object_key, tag_key, tag_value) VALUES (?, ?, ?)`,
		objectKey, tagKey, tagValue,
	); err != nil {
		return fmt.Errorf("insert object tag: %w", err)
	}
	return nil
}

// DeleteObjectTags removes every tag row for one object key.
func (a *sqliteTxAdapter) DeleteObjectTags(ctx context.Context, objectKey string) error {
	if _, err := a.tx.ExecContext(ctx,
		`DELETE FROM object_tags WHERE object_key = ?`, objectKey,
	); err != nil {
		return fmt.Errorf("delete object tags: %w", err)
	}
	return nil
}

// DeleteObjectTagsForKeys removes every tag row for any of the given keys,
// passing the list as JSON for the same reason DeleteObjectsByKeys does:
// SQLite has no array parameter, and json_each keeps this one statement
// rather than one per key.
func (a *sqliteTxAdapter) DeleteObjectTagsForKeys(ctx context.Context, objectKeys []string) error {
	if len(objectKeys) == 0 {
		return nil
	}
	keysJSON, err := json.Marshal(objectKeys)
	if err != nil {
		return fmt.Errorf("marshal keys: %w", err)
	}
	if _, err := a.tx.ExecContext(ctx, `
		DELETE FROM object_tags
		WHERE object_key IN (SELECT value FROM json_each(?))`, string(keysJSON)); err != nil {
		return fmt.Errorf("delete object tags for keys: %w", err)
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
// Returns the inserted size_bytes (read from the locked source row, so
// it agrees with whatever object_locations.size_bytes the SQLite row
// got) on success, or (0, false, nil) when the source is missing or the
// target already has a copy.
func (a *sqliteTxAdapter) InsertReplicaConditional(ctx context.Context, objectKey, targetBackend, sourceBackend string) (int64, bool, error) {
	srcLoc, ok, err := a.LockObjectOnBackend(ctx, objectKey, sourceBackend)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, false, nil
	}
	targetExists, err := a.CheckObjectExistsOnBackend(ctx, objectKey, targetBackend)
	if err != nil {
		return 0, false, err
	}
	if targetExists {
		return 0, false, nil
	}
	// The whole source row is carried over rather than a hand-listed subset of
	// its fields: the replica holds the same stored bytes, so anything omitted
	// here is a column describing bytes that the copy then contradicts. That is
	// how the conditional insert came to drop every encryption field.
	dest := *srcLoc
	dest.ObjectKey = objectKey
	dest.BackendName = targetBackend
	if err := a.InsertObjectLocation(ctx, &dest); err != nil {
		return 0, false, err
	}
	return srcLoc.SizeBytes, true, nil
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

// GetCleanupQueueRow returns the full payload of a cleanup_queue row by
// id. Inside MoveCleanupToDLQ this read carries every column the DLQ
// insert needs in one round trip. Returns ErrCleanupItemNotFound when
// the row is gone (a concurrent worker already moved or completed it).
func (a *sqliteTxAdapter) GetCleanupQueueRow(ctx context.Context, id int64) (core.CleanupQueueRow, error) {
	var (
		row       core.CleanupQueueRow
		createdAt string
		lastErr   sql.NullString
	)
	err := a.tx.QueryRowContext(ctx,
		`SELECT id, backend_name, object_key, reason, size_bytes,
		        attempts, created_at, last_error
		 FROM cleanup_queue
		 WHERE id = ?`, id,
	).Scan(&row.ID, &row.BackendName, &row.ObjectKey, &row.Reason,
		&row.SizeBytes, &row.Attempts, &createdAt, &lastErr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.CleanupQueueRow{}, core.ErrCleanupItemNotFound
		}
		return core.CleanupQueueRow{}, fmt.Errorf("get cleanup queue row: %w", err)
	}
	if t, perr := time.Parse(time.RFC3339Nano, createdAt); perr == nil {
		row.CreatedAt = t
	}
	row.LastError = nullStringValue(lastErr)
	return row, nil
}

// InsertCleanupDLQ inserts row into cleanup_dlq. Bytes are not
// reconciled here because the underlying object is still on the backend;
// orphan_bytes accounting stays untouched on the move.
func (a *sqliteTxAdapter) InsertCleanupDLQ(ctx context.Context, row *core.CleanupQueueRow) error {
	firstEnqueued := formatTime(row.CreatedAt)
	if row.CreatedAt.IsZero() {
		firstEnqueued = now()
	}
	var lastErr any
	if row.LastError != "" {
		lastErr = row.LastError
	}
	if _, err := a.tx.ExecContext(ctx,
		`INSERT INTO cleanup_dlq (
			original_id, backend_name, object_key, reason, size_bytes,
			attempts, first_enqueued_at, last_error
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.BackendName, row.ObjectKey, row.Reason, row.SizeBytes,
		row.Attempts, firstEnqueued, lastErr,
	); err != nil {
		return fmt.Errorf("insert cleanup_dlq: %w", err)
	}
	return nil
}

// DeleteCleanupItem removes the cleanup_queue row by id. Used inside
// MoveCleanupToDLQ so the queue->DLQ move is atomic with the insert
// above.
func (a *sqliteTxAdapter) DeleteCleanupItem(ctx context.Context, id int64) error {
	if _, err := a.tx.ExecContext(ctx,
		`DELETE FROM cleanup_queue WHERE id = ?`, id,
	); err != nil {
		return fmt.Errorf("delete cleanup_queue row: %w", err)
	}
	return nil
}

// HasPendingCleanup reports whether a delete for (objectKey, backend) is still
// outstanding in either the retry queue or the dead-letter table.
func (a *sqliteTxAdapter) HasPendingCleanup(ctx context.Context, objectKey, backend string) (bool, error) {
	var pending bool
	err := a.tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM cleanup_queue WHERE object_key = ? AND backend_name = ?
			UNION ALL
			SELECT 1 FROM cleanup_dlq   WHERE object_key = ? AND backend_name = ?
		)`, objectKey, backend, objectKey, backend).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("check pending cleanup: %w", err)
	}
	return pending, nil
}

// -------------------------------------------------------------------------
// QUOTA TX OPERATIONS
// -------------------------------------------------------------------------

// IncrementBackendQuota credits delta bytes to backendName. Returns
// core.ErrNoSpaceAvailable when the guarded UPDATE touches zero rows
// (quota ceiling would be exceeded). orphan_bytes counts toward the ceiling
// because those bytes are still on the backend until their cleanup lands, and
// target selection already declines them; leaving them out here would admit
// writes the placement layer had already ruled out.
func (a *sqliteTxAdapter) IncrementBackendQuota(ctx context.Context, backendName string, delta int64) error {
	now := now()
	res, err := a.tx.ExecContext(ctx, `
		UPDATE backend_quotas
		SET bytes_used = bytes_used + ?, updated_at = ?
		WHERE backend_name = ?
		  AND (bytes_limit = 0 OR bytes_used + orphan_bytes + ? <= bytes_limit)`,
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
	now := now()
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
	now := now()
	if _, err := a.tx.ExecContext(ctx, `
		UPDATE backend_quotas
		SET orphan_bytes = MAX(0, orphan_bytes - ?), updated_at = ?
		WHERE backend_name = ?`, delta, now, backendName); err != nil {
		return fmt.Errorf("decrement orphan bytes: %w", err)
	}
	return nil
}

// AllBackendBytesUsed returns the current bytes_used for every
// backend_quotas row, keyed by backend name.
func (a *sqliteTxAdapter) AllBackendBytesUsed(ctx context.Context) (map[string]int64, error) {
	rows, err := a.tx.QueryContext(ctx, `SELECT backend_name, bytes_used FROM backend_quotas`)
	if err != nil {
		return nil, fmt.Errorf("read all bytes_used: %w", err)
	}
	return collectMap(rows, "bytes_used", scanNameValue)
}

// SumObjectSizesByBackend returns SUM(size_bytes) per backend from the
// object_locations ledger, keyed by backend name.
func (a *sqliteTxAdapter) SumObjectSizesByBackend(ctx context.Context) (map[string]int64, error) {
	rows, err := a.tx.QueryContext(ctx,
		`SELECT backend_name, COALESCE(SUM(size_bytes), 0) FROM object_locations GROUP BY backend_name`)
	if err != nil {
		return nil, fmt.Errorf("sum object sizes by backend: %w", err)
	}
	return collectMap(rows, "object size sums", scanNameValue)
}

// SetBackendBytesUsed overwrites bytes_used with the authoritative value.
func (a *sqliteTxAdapter) SetBackendBytesUsed(ctx context.Context, backendName string, value int64) error {
	now := now()
	if _, err := a.tx.ExecContext(ctx, `
		UPDATE backend_quotas
		SET bytes_used = ?, updated_at = ?
		WHERE backend_name = ?`, value, now, backendName); err != nil {
		return fmt.Errorf("set backend bytes_used: %w", err)
	}
	return nil
}

// AdjustBackendBytesUsed applies a signed delta to bytes_used. MAX(0, ...)
// clamps it: a stale size must not leave the counter negative, which would
// over-admit every later write.
func (a *sqliteTxAdapter) AdjustBackendBytesUsed(ctx context.Context, backendName string, delta int64) error {
	if _, err := a.tx.ExecContext(ctx, `
		UPDATE backend_quotas
		SET bytes_used = MAX(0, bytes_used + ?), updated_at = ?
		WHERE backend_name = ?`, delta, now(), backendName); err != nil {
		return fmt.Errorf("adjust backend bytes_used: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// STORED-FORM REWRITES
// -------------------------------------------------------------------------

// UpdateCompressedForm records the stored form a recompression pass left on
// one copy. The envelope columns are rewritten too: re-encrypting mints a new
// base nonce and wrapped key, so leaving the old ones would describe bytes
// nothing can decrypt.
func (a *sqliteTxAdapter) UpdateCompressedForm(ctx context.Context, u *core.CompressedUpdate) error {
	if _, err := a.tx.ExecContext(ctx, `
		UPDATE object_locations
		SET compression_algorithm = ?, compression_level = ?,
		    compression_format_version = ?, logical_size = ?,
		    size_bytes = ?, plaintext_size = ?,
		    encryption_key = ?, key_id = ?
		WHERE object_key = ? AND backend_name = ?`,
		nullableString(u.Algorithm), nullableString(u.Level),
		nullableInt64(int64(u.FormatVersion)), nullableInt64(u.LogicalSize),
		u.SizeBytes, nullableInt64(u.PlaintextSize),
		u.EncryptionKey, nullableString(u.KeyID),
		u.ObjectKey, u.BackendName,
	); err != nil {
		return fmt.Errorf("update compressed form: %w", err)
	}
	return nil
}

// MarkCopyEncrypted records the envelope columns for a copy encrypted in place.
func (a *sqliteTxAdapter) MarkCopyEncrypted(ctx context.Context, u *core.EncryptedUpdate) error {
	if _, err := a.tx.ExecContext(ctx, `
		UPDATE object_locations
		SET encrypted = 1, encryption_key = ?, key_id = ?,
		    plaintext_size = ?, size_bytes = ?
		WHERE object_key = ? AND backend_name = ?`,
		u.EncryptionKey, u.KeyID, u.PlaintextSize, u.CiphertextSize, u.ObjectKey, u.BackendName,
	); err != nil {
		return fmt.Errorf("mark copy encrypted: %w", err)
	}
	return nil
}

// MarkCopyDecrypted clears the envelope columns for a copy decrypted in place.
func (a *sqliteTxAdapter) MarkCopyDecrypted(ctx context.Context, objectKey, backendName string, plaintextSize int64) error {
	if _, err := a.tx.ExecContext(ctx, `
		UPDATE object_locations
		SET encrypted = 0, encryption_key = NULL, key_id = NULL,
		    plaintext_size = NULL, size_bytes = ?
		WHERE object_key = ? AND backend_name = ?`,
		plaintextSize, objectKey, backendName,
	); err != nil {
		return fmt.Errorf("mark copy decrypted: %w", err)
	}
	return nil
}

// GetCopySizeBytes reads the size a copy currently reports.
func (a *sqliteTxAdapter) GetCopySizeBytes(ctx context.Context, objectKey, backendName string) (int64, error) {
	var size int64
	if err := a.tx.QueryRowContext(ctx, `
		SELECT size_bytes FROM object_locations
		WHERE object_key = ? AND backend_name = ?`,
		objectKey, backendName,
	).Scan(&size); err != nil {
		return 0, fmt.Errorf("get copy size_bytes: %w", err)
	}
	return size, nil
}

// Compile-time check that *sqliteTxAdapter satisfies core.TxAdapter.
var _ core.TxAdapter = (*sqliteTxAdapter)(nil)
