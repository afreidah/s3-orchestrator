// -------------------------------------------------------------------------------
// SQLite Multipart Uploads - Create, Part, Complete, Abort Operations
//
// Author: Alex Freidah
//
// Implements multipart upload lifecycle operations for the SQLite backend:
// create uploads, record parts with upsert, list uploads by prefix or backend,
// count active uploads, and fetch stale uploads for cleanup.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// UPLOAD LIFECYCLE
// -------------------------------------------------------------------------

// CreateMultipartUpload records a new multipart upload in the database.
func (s *Store) CreateMultipartUpload(ctx context.Context, params *core.CreateMultipartUploadParams) error {
	var metaJSON []byte
	if len(params.Metadata) > 0 {
		var err error
		metaJSON, err = json.Marshal(params.Metadata)
		if err != nil {
			return fmt.Errorf("failed to marshal metadata: %w", err)
		}
	}

	now := now()
	var encKey any
	if len(params.EncryptionKey) > 0 {
		encKey = params.EncryptionKey
	}
	var keyID any
	if params.KeyID != "" {
		keyID = params.KeyID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO multipart_uploads (upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, tagging, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		params.UploadID, params.ObjectKey, params.BackendName, params.ContentType, string(metaJSON), encKey, keyID,
		core.EncodeTags(params.Tags), now,
	)
	if err != nil {
		return fmt.Errorf("failed to create multipart upload: %w", err)
	}
	return nil
}

// GetMultipartUpload retrieves metadata for a multipart upload.
func (s *Store) GetMultipartUpload(ctx context.Context, uploadID string) (*core.MultipartUpload, error) {
	// tagging is read here and nowhere else: CompleteMultipartUpload applies
	// the set the create call carried, and the list paths have no use for it.
	// Selected separately from the shared scanner so those paths keep their
	// column list, and so both engines populate Tags on the same one read.
	row := s.db.QueryRowContext(ctx,
		`SELECT upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, created_at, tagging
		 FROM multipart_uploads
		 WHERE upload_id = ?`,
		uploadID,
	)
	var tagging sql.NullString
	mu, err := scanMultipartUploadRow(taggedRowScanner{row: row, tagging: &tagging})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrMultipartUploadNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get multipart upload: %w", err)
	}
	if mu.Tags, err = core.DecodeTags(nullStringValue(tagging)); err != nil {
		return nil, err
	}
	return &mu, nil
}

// -------------------------------------------------------------------------
// PARTS
// -------------------------------------------------------------------------

// RecordPart records a completed part for a multipart upload. Re-uploading the
// same part number updates the existing row (ON CONFLICT DO UPDATE).
func (s *Store) RecordPart(ctx context.Context, p *core.RecordPartParams) error {
	if p.PartNumber < 1 || p.PartNumber > 10000 {
		return fmt.Errorf("invalid part number %d: must be between 1 and 10000", p.PartNumber)
	}

	now := now()

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

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO multipart_parts (upload_id, part_number, etag, plaintext_etag, size_bytes, encrypted, encryption_key, key_id, plaintext_size, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (upload_id, part_number) DO UPDATE SET
		     etag = excluded.etag,
		     plaintext_etag = excluded.plaintext_etag,
		     size_bytes = excluded.size_bytes,
		     encrypted = excluded.encrypted,
		     encryption_key = excluded.encryption_key,
		     key_id = excluded.key_id,
		     plaintext_size = excluded.plaintext_size,
		     created_at = excluded.created_at`,
		p.UploadID, p.PartNumber, p.ETag, nullableString(p.PlaintextETag), p.SizeBytes, encrypted, encryptionKey, keyID, plaintextSize, now,
	)
	if err != nil {
		return fmt.Errorf("failed to record part: %w", err)
	}
	return nil
}

// GetParts returns all parts for a multipart upload, ordered by part number.
func (s *Store) GetParts(ctx context.Context, uploadID string) ([]core.MultipartPart, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT part_number, etag, plaintext_etag, size_bytes, encrypted, encryption_key, key_id, plaintext_size, created_at
		 FROM multipart_parts
		 WHERE upload_id = ?
		 ORDER BY part_number`,
		uploadID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get parts: %w", err)
	}
	return collectRows(rows, "parts", func(rows *sql.Rows) (core.MultipartPart, error) {
		var (
			p         core.MultipartPart
			ptETag    sql.NullString
			keyID     sql.NullString
			ptSize    sql.NullInt64
			createdAt string
		)
		if err := rows.Scan(&p.PartNumber, &p.ETag, &ptETag, &p.SizeBytes, &p.Encrypted, &p.EncryptionKey, &keyID, &ptSize, &createdAt); err != nil {
			return core.MultipartPart{}, fmt.Errorf("failed to scan part: %w", err)
		}
		p.PlaintextETag = nullStringValue(ptETag)
		p.KeyID = nullStringValue(keyID)
		p.PlaintextSize = nullInt64Value(ptSize)
		created, err := parseTime(createdAt)
		if err != nil {
			return core.MultipartPart{}, fmt.Errorf("invalid part created_at timestamp %q: %w", createdAt, err)
		}
		p.CreatedAt = created
		return p, nil
	})
}

// -------------------------------------------------------------------------
// DELETION AND LISTING
// -------------------------------------------------------------------------

// DeleteMultipartUpload removes a multipart upload and its parts. Parts are
// deleted first to satisfy foreign key constraints, then the upload row.
func (s *Store) DeleteMultipartUpload(ctx context.Context, uploadID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM multipart_parts WHERE upload_id = ?`, uploadID); err != nil {
			return fmt.Errorf("failed to delete parts: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM multipart_uploads WHERE upload_id = ?`, uploadID); err != nil {
			return fmt.Errorf("failed to delete multipart upload: %w", err)
		}
		return nil
	})
}

// ListMultipartUploads returns in-progress multipart uploads whose key matches
// the given prefix, up to maxUploads entries.
func (s *Store) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]core.MultipartUpload, error) {
	escapedPrefix := likeEscape(prefix)

	rows, err := s.db.QueryContext(ctx,
		`SELECT upload_id, object_key, content_type, created_at
		 FROM multipart_uploads
		 WHERE object_key LIKE ? || '%' ESCAPE '\'
		 ORDER BY object_key, created_at
		 LIMIT ?`,
		escapedPrefix, maxUploads,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list multipart uploads: %w", err)
	}
	return collectRows(rows, "multipart uploads", func(rows *sql.Rows) (core.MultipartUpload, error) {
		var (
			mu          core.MultipartUpload
			contentType sql.NullString
			createdAt   string
		)
		if err := rows.Scan(&mu.UploadID, &mu.ObjectKey, &contentType, &createdAt); err != nil {
			return core.MultipartUpload{}, fmt.Errorf("failed to scan multipart upload: %w", err)
		}
		mu.ContentType = nullStringValue(contentType)
		created, err := parseTime(createdAt)
		if err != nil {
			return core.MultipartUpload{}, fmt.Errorf(errInvalidTimestamp, createdAt, err)
		}
		mu.CreatedAt = created
		return mu, nil
	})
}

// -------------------------------------------------------------------------
// COUNTS AND HOUSEKEEPING
// -------------------------------------------------------------------------

// CountActiveMultipartUploads returns the number of in-progress multipart
// uploads whose key starts with the given bucket prefix.
func (s *Store) CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error) {
	return s.countRows(ctx, "active multipart uploads",
		`SELECT COUNT(*) FROM multipart_uploads
		 WHERE object_key LIKE ? || '%' ESCAPE '\'`,
		likeEscape(bucketPrefix))
}

// GetStaleMultipartUploads returns uploads older than the given duration.
func (s *Store) GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]core.MultipartUpload, error) {
	cutoff := formatTime(time.Now().Add(-olderThan))

	rows, err := s.db.QueryContext(ctx,
		`SELECT upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, created_at
		 FROM multipart_uploads
		 WHERE created_at < ?`,
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get stale uploads: %w", err)
	}
	defer rows.Close()

	return scanMultipartUploads(rows)
}

// GetMultipartUploadsByBackend returns all in-progress multipart uploads on
// the given backend. Used by drain to abort uploads before migrating objects.
func (s *Store) GetMultipartUploadsByBackend(ctx context.Context, backendName string) ([]core.MultipartUpload, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, created_at
		 FROM multipart_uploads
		 WHERE backend_name = ?`,
		backendName,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get multipart uploads by backend: %w", err)
	}
	defer rows.Close()

	return scanMultipartUploads(rows)
}

// GetActiveMultipartCounts returns the number of in-progress multipart uploads
// per backend.
func (s *Store) GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT backend_name, COUNT(*) AS upload_count
		 FROM multipart_uploads
		 GROUP BY backend_name`,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query multipart counts: %w", err)
	}
	return collectMap(rows, "multipart counts", scanNameValue)
}

// -------------------------------------------------------------------------
// ROW SCANNERS
// -------------------------------------------------------------------------

// rowScanner is the common subset of *sql.Row and *sql.Rows used by
// scanMultipartUploadRow, so single-row and multi-row callers share one
// column-mapping body.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanMultipartUploadRow scans the standard multipart_uploads column set
// (upload_id, object_key, backend_name, content_type, metadata, created_at)
// from any sql Scan-capable source and returns a MultipartUpload. Returns
// sql.ErrNoRows untouched so single-row callers can map it to a sentinel.
// taggedRowScanner adapts a row carrying one extra trailing column onto the
// shared multipart scanner, so the read that needs tagging does not fork the
// scan logic the other reads share.
type taggedRowScanner struct {
	row     rowScanner
	tagging *sql.NullString
}

// Scan appends the extra destination the wrapped row was selected with.
func (t taggedRowScanner) Scan(dest ...any) error {
	return t.row.Scan(append(dest, t.tagging)...)
}

func scanMultipartUploadRow(s rowScanner) (core.MultipartUpload, error) {
	var (
		mu            core.MultipartUpload
		contentType   sql.NullString
		metaJSON      sql.NullString
		encryptionKey []byte
		keyID         sql.NullString
		createdAt     string
	)
	if err := s.Scan(&mu.UploadID, &mu.ObjectKey, &mu.BackendName, &contentType, &metaJSON, &encryptionKey, &keyID, &createdAt); err != nil {
		return core.MultipartUpload{}, err
	}
	mu.ContentType = nullStringValue(contentType)
	if metaJSON.Valid && metaJSON.String != "" {
		if err := json.Unmarshal([]byte(metaJSON.String), &mu.Metadata); err != nil {
			return core.MultipartUpload{}, fmt.Errorf("failed to unmarshal metadata: %w", err)
		}
	}
	if len(encryptionKey) > 0 {
		mu.EncryptionKey = encryptionKey
		mu.Encrypted = true
	}
	mu.KeyID = nullStringValue(keyID)
	var parseErr error
	mu.CreatedAt, parseErr = parseTime(createdAt)
	if parseErr != nil {
		return core.MultipartUpload{}, fmt.Errorf(errInvalidTimestamp, createdAt, parseErr)
	}
	return mu, nil
}

// scanMultipartUploads loops sql.Rows through scanMultipartUploadRow,
// surfacing the standard "failed to scan" error wrap on per-row failures.
func scanMultipartUploads(rows *sql.Rows) ([]core.MultipartUpload, error) {
	return collectRows(rows, "multipart uploads", func(rows *sql.Rows) (core.MultipartUpload, error) {
		mu, err := scanMultipartUploadRow(rows)
		if err != nil {
			return core.MultipartUpload{}, fmt.Errorf("failed to scan multipart upload: %w", err)
		}
		return mu, nil
	})
}

// likeEscape escapes SQL LIKE wildcards in prefix strings.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
