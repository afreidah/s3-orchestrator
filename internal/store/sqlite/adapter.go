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
// *sql.Tx everywhere except direct. instanceID is the claim stamped on the
// cleanup rows it queues.
type sqliteTxAdapter struct {
	q          sqlQuerier
	instanceID string
}

// direct returns an adapter over the store's own db, outside any
// transaction. Store methods that run one statement delegate to it so each
// statement has one implementation.
func (s *Store) direct() *sqliteTxAdapter {
	return &sqliteTxAdapter{q: s.db, instanceID: s.instanceID}
}

// AcquireKeyLock is a silent no-op on SQLite. The engine serializes
// writers, so the in-tx existence checks in ClaimPending and
// GetExistingCopiesForUpdate provide the equivalent guarantee that
// pg_advisory_xact_lock provides on Postgres.
func (a *sqliteTxAdapter) AcquireKeyLock(context.Context, ...string) error {
	return nil
}

// -------------------------------------------------------------------------
// PENDING TX OPERATIONS
// -------------------------------------------------------------------------

// ClaimPending reports whether a pending row exists for the given
// intent. Inside a writer-serialized tx, presence is the same guarantee
// Postgres gets from SELECT FOR UPDATE.
func (a *sqliteTxAdapter) ClaimPending(ctx context.Context, intentID string) (bool, error) {
	return a.exists(ctx, "claim pending", `SELECT 1 FROM pending_objects WHERE intent_id = ?`, intentID)
}

// exists reports whether query selects a row, wrapping a failure as
// "what: err".
func (a *sqliteTxAdapter) exists(ctx context.Context, what, query string, args ...any) (bool, error) {
	var probe int
	err := a.q.QueryRowContext(ctx, query, args...).Scan(&probe)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", what, err)
	}
	return true, nil
}

// ClearPendingForKey removes the keys' intents apart from the ones the caller
// is committing, reporting each so its bytes can be cleaned off the backend
// after the transaction commits.
func (a *sqliteTxAdapter) ClearPendingForKey(ctx context.Context, keep []string, objectKeys ...string) ([]core.SupersededIntent, error) {
	rows, err := a.q.QueryContext(ctx,
		`DELETE FROM pending_objects
		  WHERE object_key IN (SELECT value FROM json_each(?))
		    AND intent_id NOT IN (SELECT value FROM json_each(?))
		 RETURNING intent_id, object_key, backend_name, storage_key, size_bytes`,
		jsonList(objectKeys), jsonList(keep))
	if err != nil {
		return nil, fmt.Errorf("clear pending intents for key: %w", err)
	}
	return collectRows(rows, "superseded intents", func(rows *sql.Rows) (core.SupersededIntent, error) {
		var si core.SupersededIntent
		if err := rows.Scan(&si.IntentID, &si.ObjectKey, &si.BackendName, &si.StorageKey, &si.SizeBytes); err != nil {
			return core.SupersededIntent{}, fmt.Errorf("scan superseded intent: %w", err)
		}
		return si, nil
	})
}

// DeletePending removes a pending intent.
func (a *sqliteTxAdapter) DeletePending(ctx context.Context, intentID string) error {
	return a.exec(ctx, "delete pending object", `DELETE FROM pending_objects WHERE intent_id = ?`, intentID)
}

// -------------------------------------------------------------------------
// OBJECTS TX OPERATIONS
// -------------------------------------------------------------------------

// GetExistingCopiesForUpdate returns every copy of the keys, passed as one
// JSON parameter via json_each. There is no FOR UPDATE: SQLite serializes
// writers, so the in-tx read already gives the same exclusivity.
func (a *sqliteTxAdapter) GetExistingCopiesForUpdate(ctx context.Context, objectKeys ...string) ([]core.ExistingCopy, error) {
	if len(objectKeys) == 0 {
		return nil, nil
	}
	rows, err := a.q.QueryContext(ctx, `
		SELECT object_key, backend_name, storage_key, size_bytes, created_at, form
		FROM object_locations
		WHERE object_key IN (SELECT value FROM json_each(?))`, jsonList(objectKeys))
	if err != nil {
		return nil, fmt.Errorf("query existing copies: %w", err)
	}
	return collectRows(rows, "existing copies", func(rows *sql.Rows) (core.ExistingCopy, error) {
		var (
			ec        core.ExistingCopy
			createdAt string
		)
		if err := rows.Scan(&ec.ObjectKey, &ec.BackendName, &ec.StorageKey, &ec.SizeBytes, &createdAt, &ec.Form); err != nil {
			return core.ExistingCopy{}, fmt.Errorf("scan existing copy: %w", err)
		}
		var err error
		if ec.CreatedAt, err = parseTime(createdAt); err != nil {
			return core.ExistingCopy{}, err
		}
		return ec, nil
	})
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
	etag, contentType, userMetadata := identityColumns(loc.Identity)
	return a.exec(ctx, "insert object location",
		`INSERT INTO object_locations
		   (object_key, backend_name, storage_key, size_bytes, encrypted, encryption_key,
		    key_id, plaintext_size, content_hash,
		    compression_algorithm, compression_level, compression_format_version, logical_size,
		    compression_probe_size, compression_probe_level,
		    etag, content_type, user_metadata,
		    managed, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		loc.ObjectKey, loc.BackendName, loc.StorageKey, loc.SizeBytes, form.Encrypted,
		form.EncryptionKey,
		nullableString(form.KeyID), nullableInt64(form.PlaintextSize), nullableString(form.ContentHash),
		nullableString(form.CompressionAlgorithm), nullableString(form.CompressionLevel),
		nullableInt64(int64(form.CompressionFormatVersion)), nullableInt64(form.LogicalSize),
		nullableInt64(loc.CompressionProbeSize), nullableString(loc.CompressionProbeLevel),
		etag, contentType, userMetadata,
		!loc.Unmanaged,
		created,
	)
}

// DeleteObjectCopies removes every object_locations row for the keys, which
// GetExistingCopiesForUpdate has already read in this transaction.
func (a *sqliteTxAdapter) DeleteObjectCopies(ctx context.Context, objectKeys ...string) error {
	return a.exec(ctx, "delete object copies",
		`DELETE FROM object_locations WHERE object_key IN (SELECT value FROM json_each(?))`, jsonList(objectKeys))
}

// objectExistsOnBackend reports whether (key, backend) is in
// object_locations.
func (a *sqliteTxAdapter) objectExistsOnBackend(ctx context.Context, objectKey, backend string) (bool, error) {
	return a.exists(ctx, "check object on backend",
		`SELECT 1 FROM object_locations WHERE object_key = ? AND backend_name = ?`, objectKey, backend)
}

// LockObjectOnBackend reads the (key, backend) row whole inside the
// writer-serialized tx, so a copy placed from it carries every column.
// (nil, false, nil) means the row is gone.
func (a *sqliteTxAdapter) LockObjectOnBackend(ctx context.Context, objectKey, backend string) (*core.ObjectLocation, bool, error) {
	rows, err := a.q.QueryContext(ctx, `
		SELECT `+objectColumns+`
		FROM object_locations
		WHERE object_key = ? AND backend_name = ?`, objectKey, backend)
	if err != nil {
		return nil, false, fmt.Errorf("lock object on backend: %w", err)
	}
	locs, err := collectRows(rows, "lock object on backend", scanObjectLocation)
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
		backend, jsonList(paths))
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
	return a.exec(ctx, "delete object from backend",
		`DELETE FROM object_locations WHERE object_key = ? AND backend_name = ?`, objectKey, backend)
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
	changed, err := a.execChanged(ctx, "update copy metadata",
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
	if err == nil && !changed {
		return core.ErrCopyChanged
	}
	return err
}

// -------------------------------------------------------------------------
// OBJECT TAGS
// -------------------------------------------------------------------------

// InsertObjectTag adds one tag row for an object.
func (a *sqliteTxAdapter) InsertObjectTag(ctx context.Context, objectKey, tagKey, tagValue string) error {
	return a.exec(ctx, "insert object tag",
		`INSERT INTO object_tags (object_key, tag_key, tag_value) VALUES (?, ?, ?)`, objectKey, tagKey, tagValue)
}

// DeleteObjectTags removes every tag row for any of the keys. SQLite has no
// array parameter, and json_each keeps this one statement rather than one per
// key.
func (a *sqliteTxAdapter) DeleteObjectTags(ctx context.Context, objectKeys ...string) error {
	return a.exec(ctx, "delete object tags",
		`DELETE FROM object_tags WHERE object_key IN (SELECT value FROM json_each(?))`, jsonList(objectKeys))
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

// -------------------------------------------------------------------------
// CLEANUP TX OPERATIONS
// -------------------------------------------------------------------------

// InsertCleanup queues the requests' bytes for deletion, claimed by this
// instance, and returns the rows with the claim they were stamped with. Times
// are written in the canonical fixed-width format, so the claim a settle
// matches is exactly the text stored. Each row is its own statement, since
// SQLite runs in-process and a statement costs no round trip.
func (a *sqliteTxAdapter) InsertCleanup(ctx context.Context, requests ...*core.CleanupRequest) ([]core.CleanupItem, error) {
	claimedAt := time.Now()
	stamp := formatTime(claimedAt)
	items := make([]core.CleanupItem, len(requests))
	for i, c := range requests {
		var id int64
		if err := a.q.QueryRowContext(ctx,
			`INSERT INTO cleanup_queue
			   (backend_name, object_key, storage_key, reason, size_bytes, created_at, next_retry, claimed_at, claimed_by)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			 RETURNING id`,
			c.BackendName, c.ObjectKey, c.StorageKey, c.Reason, c.SizeBytes,
			stamp, stamp, stamp, nullableString(a.instanceID),
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("insert cleanup row: %w", err)
		}
		items[i] = core.CleanupItem{
			CleanupRequest: *c,
			ID:             id,
			Claim:          &core.CleanupClaim{At: claimedAt, By: a.instanceID},
		}
	}
	return items, nil
}

// execRows runs a write and reports how many rows it changed. what prefixes
// either failure.
func (a *sqliteTxAdapter) execRows(ctx context.Context, what, query string, args ...any) (int64, error) {
	res, err := a.q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	return n, nil
}

// execChanged runs a write and reports whether it changed any row, for the
// conditional inserts and updates whose outcome is the answer.
func (a *sqliteTxAdapter) execChanged(ctx context.Context, what, query string, args ...any) (bool, error) {
	n, err := a.execRows(ctx, what, query, args...)
	return n > 0, err
}

// exec runs a write whose row count does not matter, wrapping a failure as
// "what: err".
func (a *sqliteTxAdapter) exec(ctx context.Context, what, query string, args ...any) error {
	if _, err := a.q.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// -------------------------------------------------------------------------
// QUOTA TX OPERATIONS
// -------------------------------------------------------------------------

// AdjustQuotaStripes applies signed deltas to a backend's byte-counter stripes,
// materializing each row on first use so nothing has to seed a backend's
// stripes up front. No clamp: a stripe is signed, and the total it contributes
// to is what gets clamped when read.
func (a *sqliteTxAdapter) AdjustQuotaStripes(ctx context.Context, charges ...core.StripeCharge) error {
	for _, c := range charges {
		if err := a.exec(ctx, "adjust quota stripe for "+c.Backend, `
			INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used)
			VALUES (?, ?, ?)
			ON CONFLICT (backend_name, stripe_id) DO UPDATE
			SET bytes_used = bytes_used + excluded.bytes_used`,
			c.Backend, c.Stripe, c.Delta); err != nil {
			return err
		}
	}
	return nil
}

// AcquireBackendLock is a no-op on SQLite: the engine serializes writers, so a
// claim's room check already sees every claim committed before it.
func (a *sqliteTxAdapter) AcquireBackendLock(context.Context, ...string) error {
	return nil
}

// -------------------------------------------------------------------------
// MULTIPART TX OPERATIONS
// -------------------------------------------------------------------------

// LockMultipartUpload reads the upload's backend and when a running
// completion last renewed its mark. SQLite serializes writers, so the row
// cannot change before the transaction ends, whatever the mode. An upload
// already gone returns an empty backend.
func (a *sqliteTxAdapter) LockMultipartUpload(ctx context.Context, uploadID string, _ core.UploadLock) (string, time.Time, error) {
	var (
		backendName string
		heartbeat   sql.NullString
	)
	err := a.q.QueryRowContext(ctx,
		`SELECT backend_name, completion_heartbeat_at FROM multipart_uploads WHERE upload_id = ?`, uploadID,
	).Scan(&backendName, &heartbeat)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read multipart upload: %w", err)
	}
	t, err := parseNullableTime(heartbeat)
	if err != nil || t == nil {
		return backendName, time.Time{}, err
	}
	return backendName, *t, nil
}

// DeleteMultipartUpload removes an upload's parts and then the upload, and
// returns the parts removed.
func (a *sqliteTxAdapter) DeleteMultipartUpload(ctx context.Context, uploadID string) ([]core.MultipartPart, error) {
	rows, err := a.q.QueryContext(ctx,
		`DELETE FROM multipart_parts WHERE upload_id = ? RETURNING part_number, size_bytes, storage_key`, uploadID)
	if err != nil {
		return nil, fmt.Errorf("delete multipart parts: %w", err)
	}
	parts, err := collectRows(rows, "multipart parts", scanPartLocation)
	if err != nil {
		return nil, err
	}
	if err := a.exec(ctx, "delete multipart upload", `DELETE FROM multipart_uploads WHERE upload_id = ?`, uploadID); err != nil {
		return nil, err
	}
	return parts, nil
}

// UpsertPart records a part at its intent's path and returns the part of that
// number it replaced, or nil when there was none. SQLite serializes writers,
// so the row read is the one the write replaces.
func (a *sqliteTxAdapter) UpsertPart(ctx context.Context, p *core.RecordPartParams) (*core.MultipartPart, error) {
	rows, err := a.q.QueryContext(ctx,
		`SELECT part_number, size_bytes, storage_key FROM multipart_parts WHERE upload_id = ? AND part_number = ?`,
		p.UploadID, p.PartNumber)
	if err != nil {
		return nil, fmt.Errorf("read replaced part: %w", err)
	}
	prev, err := collectRows(rows, "replaced part", scanPartLocation)
	if err != nil {
		return nil, err
	}

	var (
		encrypted     bool
		encryptionKey []byte
		keyID         *string
		plaintextSize *int64
	)
	if p.Form != nil && p.Form.Encrypted {
		encrypted = true
		encryptionKey = p.Form.EncryptionKey
		keyID = &p.Form.KeyID
		plaintextSize = &p.Form.PlaintextSize
	}
	if err := a.exec(ctx, "record part",
		`INSERT INTO multipart_parts (upload_id, part_number, etag, plaintext_etag, size_bytes, encrypted, encryption_key, key_id, plaintext_size, storage_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (upload_id, part_number) DO UPDATE SET
		     etag = excluded.etag,
		     plaintext_etag = excluded.plaintext_etag,
		     size_bytes = excluded.size_bytes,
		     encrypted = excluded.encrypted,
		     encryption_key = excluded.encryption_key,
		     key_id = excluded.key_id,
		     plaintext_size = excluded.plaintext_size,
		     storage_key = excluded.storage_key,
		     created_at = excluded.created_at`,
		p.UploadID, p.PartNumber, p.ETag, nullableString(p.PlaintextETag), p.SizeBytes,
		encrypted, encryptionKey, keyID, plaintextSize, nullableString(p.Intent.StorageKey), now(),
	); err != nil {
		return nil, err
	}
	if len(prev) == 0 {
		return nil, nil
	}
	return &prev[0], nil
}

// scanPartLocation reads a part's number, size and storage path, the columns a
// part's cleanup needs.
func scanPartLocation(rows *sql.Rows) (core.MultipartPart, error) {
	var (
		p          core.MultipartPart
		storageKey sql.NullString
	)
	if err := rows.Scan(&p.PartNumber, &p.SizeBytes, &storageKey); err != nil {
		return core.MultipartPart{}, fmt.Errorf("scan part location: %w", err)
	}
	p.StorageKey = nullStringValue(storageKey)
	return p, nil
}

// Compile-time check that *sqliteTxAdapter satisfies core.TxAdapter.
var _ core.TxAdapter = (*sqliteTxAdapter)(nil)
