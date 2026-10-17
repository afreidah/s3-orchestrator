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

// sqlQuerier is the statement surface the adapter uses. Both *sql.Tx and
// the store's breaker-wrapped db satisfy it.
type sqlQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sqliteTxAdapter implements core.TxAdapter over a sqlQuerier, which is a
// *sql.Tx everywhere except direct.
type sqliteTxAdapter struct {
	q sqlQuerier
}

// direct returns an adapter over the store's own db, outside any
// transaction. Store methods that run one statement delegate to it so each
// statement has one implementation.
func (s *Store) direct() *sqliteTxAdapter {
	return &sqliteTxAdapter{q: s.db}
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
	err := a.q.QueryRowContext(ctx,
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

// ClearPendingForKey removes the key's intents apart from the ones the caller
// is committing, reporting each so its bytes can be cleaned off the backend
// after the transaction commits.
//
// Read then delete rather than DELETE ... RETURNING: SQLite serializes writers
// for the length of the transaction, so nothing can insert an intent for this
// key between the two statements.
func (a *sqliteTxAdapter) ClearPendingForKey(ctx context.Context, objectKey string, keep []string) ([]core.SupersededIntent, error) {
	if keep == nil {
		keep = []string{}
	}
	keepJSON, err := json.Marshal(keep)
	if err != nil {
		return nil, fmt.Errorf("marshal kept intents: %w", err)
	}
	rows, err := a.q.QueryContext(ctx,
		`SELECT intent_id, backend_name, storage_key, size_bytes
		   FROM pending_objects
		  WHERE object_key = ? AND intent_id NOT IN (SELECT value FROM json_each(?))`,
		objectKey, string(keepJSON))
	if err != nil {
		return nil, fmt.Errorf("read superseded intents: %w", err)
	}
	cleared, err := collectRows(rows, "superseded intents", func(rows *sql.Rows) (core.SupersededIntent, error) {
		var si core.SupersededIntent
		err := rows.Scan(&si.IntentID, &si.BackendName, &si.StorageKey, &si.SizeBytes)
		return si, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan superseded intents: %w", err)
	}
	if len(cleared) == 0 {
		return nil, nil
	}
	if _, err := a.q.ExecContext(ctx,
		`DELETE FROM pending_objects
		  WHERE object_key = ? AND intent_id NOT IN (SELECT value FROM json_each(?))`,
		objectKey, string(keepJSON)); err != nil {
		return nil, fmt.Errorf("clear pending intents for key: %w", err)
	}
	return cleared, nil
}

// DeletePending removes a pending intent.
func (a *sqliteTxAdapter) DeletePending(ctx context.Context, intentID string) error {
	if _, err := a.q.ExecContext(ctx,
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
	rows, err := a.q.QueryContext(ctx,
		`SELECT backend_name, storage_key, size_bytes, created_at, form
		 FROM object_locations
		 WHERE object_key = ?`, objectKey)
	if err != nil {
		return nil, fmt.Errorf("query existing copies: %w", err)
	}
	return collectRows(rows, "existing copies", func(rows *sql.Rows) (core.ExistingCopy, error) {
		var (
			ec        core.ExistingCopy
			createdAt string
		)
		if err := rows.Scan(&ec.BackendName, &ec.StorageKey, &ec.SizeBytes, &createdAt, &ec.Form); err != nil {
			return core.ExistingCopy{}, fmt.Errorf("scan existing copy: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			ec.CreatedAt = t
		}
		return ec, nil
	})
}

// GetCopiesForKeysForUpdate returns every (key, backend, size) row matching
// any key in the supplied list, passing the keys as one JSON parameter via
// json_each. There is no FOR UPDATE: SQLite serializes writers, so the in-tx
// read already gives the same exclusivity.
func (a *sqliteTxAdapter) GetCopiesForKeysForUpdate(ctx context.Context, keys []string) ([]core.KeyedExistingCopy, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return nil, fmt.Errorf("marshal keys: %w", err)
	}
	rows, err := a.q.QueryContext(ctx, `
		SELECT object_key, backend_name, storage_key, size_bytes
		FROM object_locations
		WHERE object_key IN (SELECT value FROM json_each(?))`, string(keysJSON))
	if err != nil {
		return nil, fmt.Errorf("get copies for keys: %w", err)
	}
	return collectRows(rows, "keyed copies", func(rows *sql.Rows) (core.KeyedExistingCopy, error) {
		var ec core.KeyedExistingCopy
		if err := rows.Scan(&ec.ObjectKey, &ec.BackendName, &ec.StorageKey, &ec.SizeBytes); err != nil {
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
	if _, err := a.q.ExecContext(ctx, `
		DELETE FROM object_locations
		WHERE object_key IN (SELECT value FROM json_each(?))`, string(keysJSON)); err != nil {
		return fmt.Errorf("delete objects by keys: %w", err)
	}
	return nil
}

// InsertObjectLocation writes a new object_locations row carrying every column
// loc describes, including the compression probe a placed copy inherits.
func (a *sqliteTxAdapter) InsertObjectLocation(ctx context.Context, loc *core.ObjectLocation) error {
	// A placed or imported copy carries the object's original write time, which
	// the row keeps so every copy reports the same Last-Modified. A fresh write
	// leaves it zero and is stamped now.
	created := now()
	if !loc.CreatedAt.IsZero() {
		created = formatTime(loc.CreatedAt)
	}
	form := loc.Recorded()
	if _, err := a.q.ExecContext(ctx,
		`INSERT INTO object_locations
		   (object_key, backend_name, storage_key, size_bytes, encrypted, encryption_key,
		    key_id, plaintext_size, content_hash,
		    compression_algorithm, compression_level, compression_format_version, logical_size,
		    compression_probe_size, compression_probe_level,
		    etag, content_type, user_metadata,
		    managed, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		loc.ObjectKey, loc.BackendName, loc.StorageKey, loc.SizeBytes, boolToInt(form.Encrypted),
		form.EncryptionKey,
		nullableString(form.KeyID), nullableInt64(form.PlaintextSize), nullableString(form.ContentHash),
		nullableString(form.CompressionAlgorithm), nullableString(form.CompressionLevel),
		nullableInt64(int64(form.CompressionFormatVersion)), nullableInt64(form.LogicalSize),
		nullableInt64(loc.CompressionProbeSize), nullableString(loc.CompressionProbeLevel),
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
	if _, err := a.q.ExecContext(ctx,
		`DELETE FROM object_locations WHERE object_key = ?`, objectKey,
	); err != nil {
		return fmt.Errorf("delete object copies: %w", err)
	}
	return nil
}

// objectExistsOnBackend reports whether (key, backend) is in
// object_locations.
func (a *sqliteTxAdapter) objectExistsOnBackend(ctx context.Context, objectKey, backend string) (bool, error) {
	var probe int
	err := a.q.QueryRowContext(ctx,
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

// LockObjectOnBackend reads the (key, backend) row whole inside the
// writer-serialized tx, through the scanner the read path uses, so a copy
// placed from it carries every column. The tag count is per key and a
// placement does not read it. (nil, false, nil) means the row is gone.
func (a *sqliteTxAdapter) LockObjectOnBackend(ctx context.Context, objectKey, backend string) (*core.ObjectLocation, bool, error) {
	rows, err := a.q.QueryContext(ctx, `
		SELECT `+objectColumns+`, etag, content_type, user_metadata, managed, 0
		FROM object_locations
		WHERE object_key = ? AND backend_name = ?`, objectKey, backend)
	if err != nil {
		return nil, false, fmt.Errorf("lock object on backend: %w", err)
	}
	locs, err := collectRows(rows, "lock object on backend", scanIdentifiedObjectLocation)
	if err != nil || len(locs) == 0 {
		return nil, false, err
	}
	return &locs[0], true, nil
}

// ListedPathStates reports what the ledger says about each listed path on the
// backend: 3 for a delete still queued or dead-lettered, 2 for a write's intent
// naming the path; its commit is still to come, 1 for a recorded copy. Untracked
// paths are absent from the map, and a path matching several arms keeps the highest state.
func (a *sqliteTxAdapter) ListedPathStates(ctx context.Context, backend string, paths []string) (map[string]core.PathState, error) {
	if len(paths) == 0 {
		return map[string]core.PathState{}, nil
	}
	pathsJSON, err := json.Marshal(paths)
	if err != nil {
		return nil, fmt.Errorf("marshal listed paths: %w", err)
	}
	rows, err := a.q.QueryContext(ctx, `
		SELECT storage_key, 3 FROM cleanup_queue
		 WHERE backend_name = ?1 AND storage_key IN (SELECT value FROM json_each(?2))
		UNION ALL
		SELECT storage_key, 3 FROM cleanup_dlq
		 WHERE backend_name = ?1 AND storage_key IN (SELECT value FROM json_each(?2))
		UNION ALL
		SELECT storage_key, 2 FROM pending_objects
		 WHERE backend_name = ?1 AND storage_key IN (SELECT value FROM json_each(?2))
		UNION ALL
		SELECT storage_key, 1 FROM object_locations
		 WHERE backend_name = ?1 AND storage_key IN (SELECT value FROM json_each(?2))`,
		backend, string(pathsJSON))
	if err != nil {
		return nil, fmt.Errorf("look up listed paths: %w", err)
	}
	defer rows.Close()
	states := make(map[string]core.PathState, len(paths))
	for rows.Next() {
		var path string
		var state core.PathState
		if err := rows.Scan(&path, &state); err != nil {
			return nil, fmt.Errorf("scan listed path: %w", err)
		}
		states[path] = max(states[path], state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate listed paths: %w", err)
	}
	return states, nil
}

// DeleteObjectFromBackend removes the single (key, backend) row.
func (a *sqliteTxAdapter) DeleteObjectFromBackend(ctx context.Context, objectKey, backend string) error {
	if _, err := a.q.ExecContext(ctx,
		`DELETE FROM object_locations WHERE object_key = ? AND backend_name = ?`,
		objectKey, backend,
	); err != nil {
		return fmt.Errorf("delete object from backend: %w", err)
	}
	return nil
}

// UpdateCopyMetadata writes a patch to the copy loc names: a NULL argument
// leaves its column as it is, and a set scrubbed stamps last_scrubbed_at.
// Matching storage_key as well means a copy a newer write replaced matches no
// row, which is reported as core.ErrCopyChanged.
func (a *sqliteTxAdapter) UpdateCopyMetadata(ctx context.Context, loc *core.ObjectLocation, p *core.CopyPatch) error {
	var probeSize sql.NullInt64
	var probeLevel sql.NullString
	if p.Probe != nil {
		probeSize = sql.NullInt64{Int64: p.Probe.Size, Valid: true}
		probeLevel = sql.NullString{String: p.Probe.Level, Valid: true}
	}
	res, err := a.q.ExecContext(ctx,
		`UPDATE object_locations
		 SET encryption_key          = COALESCE(?, encryption_key),
		     key_id                  = COALESCE(?, key_id),
		     content_hash            = COALESCE(?, content_hash),
		     last_scrubbed_at        = CASE WHEN ? THEN ? ELSE last_scrubbed_at END,
		     compression_probe_size  = COALESCE(?, compression_probe_size),
		     compression_probe_level = COALESCE(?, compression_probe_level)
		 WHERE object_key = ? AND backend_name = ? AND storage_key = ?`,
		p.EncryptionKey, nullableString(p.KeyID), nullableString(p.ContentHash),
		p.Scrubbed, now(), probeSize, probeLevel,
		loc.ObjectKey, loc.BackendName, loc.StorageKey,
	)
	if err != nil {
		return fmt.Errorf("update copy metadata: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update copy metadata: %w", err)
	}
	if n == 0 {
		return core.ErrCopyChanged
	}
	return nil
}

// -------------------------------------------------------------------------
// OBJECT TAGS
// -------------------------------------------------------------------------

// InsertObjectTag adds one tag row for an object.
func (a *sqliteTxAdapter) InsertObjectTag(ctx context.Context, objectKey, tagKey, tagValue string) error {
	if _, err := a.q.ExecContext(ctx,
		`INSERT INTO object_tags (object_key, tag_key, tag_value) VALUES (?, ?, ?)`,
		objectKey, tagKey, tagValue,
	); err != nil {
		return fmt.Errorf("insert object tag: %w", err)
	}
	return nil
}

// DeleteObjectTags removes every tag row for one object key.
func (a *sqliteTxAdapter) DeleteObjectTags(ctx context.Context, objectKey string) error {
	if _, err := a.q.ExecContext(ctx,
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
	if _, err := a.q.ExecContext(ctx, `
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
	exists, err := a.objectExistsOnBackend(ctx, loc.ObjectKey, loc.BackendName)
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

// BackendAdmits reports whether a backend accepts a write of size more bytes
// while carrying only flags within accepts, judged against backend_capacity,
// the view every admission test reads.
func (a *sqliteTxAdapter) BackendAdmits(ctx context.Context, backendName string, size int64, accepts core.BackendFlags) (bool, error) {
	var fits bool
	if err := a.q.QueryRowContext(ctx, `
		SELECT (flags & ~?) = 0 AND (available_bytes IS NULL OR available_bytes >= ?)
		FROM backend_capacity
		WHERE backend_name = ?`, accepts, size, backendName).Scan(&fits); err != nil {
		return false, fmt.Errorf("check backend headroom: %w", err)
	}
	return fits, nil
}

// -------------------------------------------------------------------------
// CLEANUP TX OPERATIONS
// -------------------------------------------------------------------------

// SumAndDeleteCleanupQueueRows deletes every cleanup_queue row for
// (storageKey, backend) and returns the count and total size of those rows.
func (a *sqliteTxAdapter) SumAndDeleteCleanupQueueRows(ctx context.Context, storageKey, backend string) (int64, int64, error) {
	var totalBytes sql.NullInt64
	var rowCount int64
	if err := a.q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes), 0), COUNT(*)
		 FROM cleanup_queue
		 WHERE storage_key = ? AND backend_name = ?`,
		storageKey, backend,
	).Scan(&totalBytes, &rowCount); err != nil {
		return 0, 0, fmt.Errorf("sum cleanup queue size: %w", err)
	}
	if rowCount == 0 {
		return 0, 0, nil
	}
	if _, err := a.q.ExecContext(ctx,
		`DELETE FROM cleanup_queue WHERE storage_key = ? AND backend_name = ?`,
		storageKey, backend,
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
	err := a.q.QueryRowContext(ctx,
		`SELECT id, backend_name, object_key, storage_key, reason, size_bytes,
		        attempts, created_at, last_error
		 FROM cleanup_queue
		 WHERE id = ?`, id,
	).Scan(&row.ID, &row.BackendName, &row.ObjectKey, &row.StorageKey, &row.Reason,
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
	if _, err := a.q.ExecContext(ctx,
		`INSERT INTO cleanup_dlq (
			original_id, backend_name, object_key, storage_key, reason, size_bytes,
			attempts, first_enqueued_at, last_error
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.BackendName, row.ObjectKey, row.StorageKey, row.Reason, row.SizeBytes,
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
	if _, err := a.q.ExecContext(ctx,
		`DELETE FROM cleanup_queue WHERE id = ?`, id,
	); err != nil {
		return fmt.Errorf("delete cleanup_queue row: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// QUOTA TX OPERATIONS
// -------------------------------------------------------------------------

// AdjustQuotaStripe applies a signed delta to one of a backend's byte-counter
// stripes, materializing the row on first use so nothing has to seed a
// backend's stripes up front. No clamp: a stripe is signed, and the total it
// contributes to is what gets clamped when read.
func (a *sqliteTxAdapter) AdjustQuotaStripe(ctx context.Context, backendName string, stripe int16, delta int64) error {
	if _, err := a.q.ExecContext(ctx, `
		INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used)
		VALUES (?, ?, ?)
		ON CONFLICT (backend_name, stripe_id) DO UPDATE
		SET bytes_used = bytes_used + excluded.bytes_used`,
		backendName, stripe, delta); err != nil {
		return fmt.Errorf("adjust quota stripe for %s: %w", backendName, err)
	}
	return nil
}

// DecrementOrphanBytes debits delta bytes from the backend's
// orphan_bytes counter (clamped at zero).
func (a *sqliteTxAdapter) DecrementOrphanBytes(ctx context.Context, backendName string, delta int64) error {
	now := now()
	if _, err := a.q.ExecContext(ctx, `
		UPDATE backend_quotas
		SET orphan_bytes = MAX(0, orphan_bytes - ?), updated_at = ?
		WHERE backend_name = ?`, delta, now, backendName); err != nil {
		return fmt.Errorf("decrement orphan bytes: %w", err)
	}
	return nil
}

// AllBackendBytesUsed returns each backend's striped byte total, keyed by
// backend name. Clamped at zero on the sum rather than per stripe, because an
// individual stripe is signed and may sit negative while the total is right.
func (a *sqliteTxAdapter) AllBackendBytesUsed(ctx context.Context) (map[string]int64, error) {
	// Driven from backend_quotas rather than the stripes: a backend that has
	// never been charged has no stripe rows, and reconciliation has to see it
	// at zero to correct it rather than skipping it entirely.
	rows, err := a.q.QueryContext(ctx, `
		SELECT q.backend_name, COALESCE(MAX(0, s.bytes_used), 0)
		FROM backend_quotas q
		LEFT JOIN (
			SELECT backend_name, SUM(bytes_used) AS bytes_used
			FROM backend_quota_stripes
			GROUP BY backend_name
		) s ON s.backend_name = q.backend_name`)
	if err != nil {
		return nil, fmt.Errorf("read all bytes_used: %w", err)
	}
	return collectMap(rows, "bytes_used", scanNameValue)
}

// SumObjectSizesByBackend returns SUM(size_bytes) per backend from the
// object_locations ledger, keyed by backend name.
func (a *sqliteTxAdapter) SumObjectSizesByBackend(ctx context.Context) (map[string]int64, error) {
	rows, err := a.q.QueryContext(ctx,
		`SELECT backend_name, COALESCE(SUM(size_bytes), 0) FROM object_locations GROUP BY backend_name`)
	if err != nil {
		return nil, fmt.Errorf("sum object sizes by backend: %w", err)
	}
	return collectMap(rows, "object size sums", scanNameValue)
}

// SetBackendBytesUsed replaces a backend's byte total with the authoritative
// recomputed value, collapsing it onto stripe zero and clearing the rest. The
// distribution that produced the old value carries no information once the
// total has been recomputed from the ledger.
func (a *sqliteTxAdapter) SetBackendBytesUsed(ctx context.Context, backendName string, value int64) error {
	if _, err := a.q.ExecContext(ctx, `
		UPDATE backend_quota_stripes SET bytes_used = 0
		WHERE backend_name = ? AND stripe_id <> 0`, backendName); err != nil {
		return fmt.Errorf("clear quota stripes: %w", err)
	}
	if _, err := a.q.ExecContext(ctx, `
		INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used)
		VALUES (?, 0, ?)
		ON CONFLICT (backend_name, stripe_id) DO UPDATE
		SET bytes_used = excluded.bytes_used`, backendName, value); err != nil {
		return fmt.Errorf("set backend bytes_used: %w", err)
	}
	return nil
}

// Compile-time check that *sqliteTxAdapter satisfies core.TxAdapter.
var _ core.TxAdapter = (*sqliteTxAdapter)(nil)
