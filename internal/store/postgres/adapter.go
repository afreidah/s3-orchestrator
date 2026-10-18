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
// transaction. instanceID is the claim stamped on the cleanup rows it queues.
type pgTxAdapter struct {
	q          *db.Queries
	instanceID string
}

// direct returns an adapter over the store's own Queries, outside any
// transaction. Store methods that run one statement delegate to it so each
// statement has one implementation.
func (s *Store) direct() *pgTxAdapter {
	return &pgTxAdapter{q: s.queries, instanceID: s.instanceID}
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

// ClearPendingForKey removes the key's intents apart from the ones the caller
// is committing, reporting each so its bytes can be cleaned off the backend
// after the transaction commits.
func (a *pgTxAdapter) ClearPendingForKey(ctx context.Context, objectKey string, keep []string) ([]core.SupersededIntent, error) {
	if keep == nil {
		keep = []string{}
	}
	rows, err := a.q.ClearPendingForKey(ctx, db.ClearPendingForKeyParams{ObjectKey: objectKey, Keep: keep})
	if err != nil {
		return nil, fmt.Errorf("clear pending intents for key: %w", err)
	}
	cleared := make([]core.SupersededIntent, len(rows))
	for i, row := range rows {
		cleared[i] = core.SupersededIntent{
			IntentID:    row.IntentID,
			BackendName: row.BackendName,
			StorageKey:  row.StorageKey,
			SizeBytes:   row.SizeBytes,
		}
	}
	return cleared, nil
}

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

// LockObjectOnBackend takes a FOR UPDATE lock on the (key, backend) row and
// returns it whole, so a copy placed from it carries every column.
// (nil, false, nil) means the row is gone.
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
	return &toWholeObjectLocations([]db.ObjectLocation{row})[0], true, nil
}

// ListedPathStates reports what the ledger says about each listed path on the
// backend. Untracked paths are absent from the map.
func (a *pgTxAdapter) ListedPathStates(ctx context.Context, backend string, paths []string) (map[string]core.PathState, error) {
	if len(paths) == 0 {
		return map[string]core.PathState{}, nil
	}
	rows, err := a.q.ListedPathStates(ctx, db.ListedPathStatesParams{BackendName: backend, Paths: paths})
	if err != nil {
		return nil, fmt.Errorf("look up listed paths: %w", err)
	}
	states := make(map[string]core.PathState, len(rows))
	for i := range rows {
		states[rows[i].StorageKey] = max(states[rows[i].StorageKey], core.PathState(rows[i].State)) //nolint:gosec // G115: state is 1, 2, or 3
	}
	return states, nil
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

// UpdateCopyMetadata writes a patch to the copy loc names. A copy a newer write
// replaced matches no row and is reported as core.ErrCopyChanged.
func (a *pgTxAdapter) UpdateCopyMetadata(ctx context.Context, loc *core.ObjectLocation, p *core.CopyPatch) error {
	params := db.UpdateCopyMetadataParams{
		EncryptionKey: p.EncryptionKey,
		KeyID:         strPtr(p.KeyID),
		ContentHash:   strPtr(p.ContentHash),
		Scrubbed:      p.Scrubbed,
		ObjectKey:     loc.ObjectKey,
		BackendName:   loc.BackendName,
		StorageKey:    loc.StorageKey,
	}
	if p.Probe != nil {
		params.ProbeSize = &p.Probe.Size
		params.ProbeLevel = &p.Probe.Level
	}
	n, err := a.q.UpdateCopyMetadata(ctx, params)
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
		StorageKey:  r.StorageKey,
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

// -------------------------------------------------------------------------
// CLEANUP TX OPERATIONS
// -------------------------------------------------------------------------

// InsertCleanup queues c's bytes for deletion, claimed by this instance, and
// returns the row's id.
func (a *pgTxAdapter) InsertCleanup(ctx context.Context, c *core.CleanupRequest) (int64, error) {
	id, err := a.q.InsertCleanup(ctx, db.InsertCleanupParams{
		BackendName: c.BackendName,
		ObjectKey:   c.ObjectKey,
		StorageKey:  core.StoragePath(c.ObjectKey, c.StorageKey),
		Reason:      c.Reason,
		SizeBytes:   c.SizeBytes,
		ClaimedBy:   strPtr(a.instanceID),
	})
	if err != nil {
		return 0, fmt.Errorf("insert cleanup row: %w", err)
	}
	return id, nil
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
		StorageKey:  row.StorageKey,
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

// InsertCleanupDLQ inserts row into cleanup_dlq, where its bytes still count
// as the backend's orphans.
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
		StorageKey:      row.StorageKey,
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
// MULTIPART TX OPERATIONS
// -------------------------------------------------------------------------

// DeleteMultipartUpload locks the upload row, removes its parts and then the
// upload, and returns the upload's backend with the parts removed. An upload
// already gone returns no parts.
func (a *pgTxAdapter) DeleteMultipartUpload(ctx context.Context, uploadID string) (string, []core.MultipartPart, error) {
	backendName, err := a.q.LockMultipartUpload(ctx, uploadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("lock multipart upload: %w", err)
	}
	rows, err := a.q.DeleteMultipartParts(ctx, uploadID)
	if err != nil {
		return "", nil, fmt.Errorf("delete multipart parts: %w", err)
	}
	if err := a.q.DeleteMultipartUpload(ctx, uploadID); err != nil {
		return "", nil, fmt.Errorf("delete multipart upload: %w", err)
	}
	parts := make([]core.MultipartPart, len(rows))
	for i, r := range rows {
		parts[i] = core.MultipartPart{PartNumber: int(r.PartNumber), SizeBytes: r.SizeBytes, StorageKey: derefStr(r.StorageKey)}
	}
	return backendName, parts, nil
}

// UpsertPart records a part at its intent's path and returns the part of that
// number it replaced, locked before the write, or nil when there was none.
func (a *pgTxAdapter) UpsertPart(ctx context.Context, p *core.RecordPartParams) (*core.MultipartPart, error) {
	var replaced *core.MultipartPart
	prev, err := a.q.GetPartForUpdate(ctx, db.GetPartForUpdateParams{
		UploadID:   p.UploadID,
		PartNumber: int32(p.PartNumber), //nolint:gosec // G115: RecordPart bounds the part number to 1..10000
	})
	switch {
	case err == nil:
		replaced = &core.MultipartPart{PartNumber: int(prev.PartNumber), SizeBytes: prev.SizeBytes, StorageKey: derefStr(prev.StorageKey)}
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("lock replaced part: %w", err)
	}
	params := db.UpsertPartParams{
		UploadID:      p.UploadID,
		PartNumber:    int32(p.PartNumber), //nolint:gosec // G115: RecordPart bounds the part number to 1..10000
		Etag:          p.ETag,
		PlaintextEtag: strPtr(p.PlaintextETag),
		SizeBytes:     p.SizeBytes,
		StorageKey:    strPtr(p.Intent.StorageKey),
	}
	if p.Form != nil && p.Form.Encrypted {
		params.Encrypted = true
		params.EncryptionKey = p.Form.EncryptionKey
		params.KeyID = &p.Form.KeyID
		params.PlaintextSize = &p.Form.PlaintextSize
	}
	if err := a.q.UpsertPart(ctx, params); err != nil {
		return nil, fmt.Errorf("record part: %w", err)
	}
	return replaced, nil
}

// Compile-time check that *pgTxAdapter satisfies core.TxAdapter.
var _ core.TxAdapter = (*pgTxAdapter)(nil)
