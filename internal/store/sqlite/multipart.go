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
	"fmt"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store"
)

// CreateMultipartUpload records a new multipart upload in the database.
func (s *Store) CreateMultipartUpload(ctx context.Context, uploadID, key, backend, contentType string, metadata map[string]string) error {
	var metaJSON []byte
	if len(metadata) > 0 {
		var err error
		metaJSON, err = json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("failed to marshal metadata: %w", err)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO multipart_uploads (upload_id, object_key, backend_name, content_type, metadata, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		uploadID, key, backend, contentType, string(metaJSON), now,
	)
	if err != nil {
		return fmt.Errorf("failed to create multipart upload: %w", err)
	}
	return nil
}

// GetMultipartUpload retrieves metadata for a multipart upload.
func (s *Store) GetMultipartUpload(ctx context.Context, uploadID string) (*store.MultipartUpload, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT upload_id, object_key, backend_name, content_type, metadata, created_at
		 FROM multipart_uploads
		 WHERE upload_id = ?`,
		uploadID,
	)

	var (
		mu          store.MultipartUpload
		contentType sql.NullString
		metaJSON    sql.NullString
		createdAt   string
	)
	err := row.Scan(&mu.UploadID, &mu.ObjectKey, &mu.BackendName, &contentType, &metaJSON, &createdAt)
	if err == sql.ErrNoRows {
		return nil, store.ErrMultipartUploadNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get multipart upload: %w", err)
	}

	if contentType.Valid {
		mu.ContentType = contentType.String
	}
	if metaJSON.Valid && metaJSON.String != "" {
		if err := json.Unmarshal([]byte(metaJSON.String), &mu.Metadata); err != nil {
			return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
		}
	}
	mu.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)

	return &mu, nil
}

// RecordPart records a completed part for a multipart upload. Re-uploading the
// same part number updates the existing row (ON CONFLICT DO UPDATE).
func (s *Store) RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, enc *store.EncryptionMeta) error {
	if partNumber < 1 || partNumber > 10000 {
		return fmt.Errorf("invalid part number %d: must be between 1 and 10000", partNumber)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	var (
		encrypted     bool
		encryptionKey []byte
		keyID         *string
		plaintextSize *int64
	)
	if enc != nil && enc.Encrypted {
		encrypted = true
		encryptionKey = enc.EncryptionKey
		keyID = &enc.KeyID
		plaintextSize = &enc.PlaintextSize
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes, encrypted, encryption_key, key_id, plaintext_size, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (upload_id, part_number) DO UPDATE SET
		     etag = excluded.etag,
		     size_bytes = excluded.size_bytes,
		     encrypted = excluded.encrypted,
		     encryption_key = excluded.encryption_key,
		     key_id = excluded.key_id,
		     plaintext_size = excluded.plaintext_size,
		     created_at = excluded.created_at`,
		uploadID, partNumber, etag, size, encrypted, encryptionKey, keyID, plaintextSize, now,
	)
	if err != nil {
		return fmt.Errorf("failed to record part: %w", err)
	}
	return nil
}

// GetParts returns all parts for a multipart upload, ordered by part number.
func (s *Store) GetParts(ctx context.Context, uploadID string) ([]store.MultipartPart, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT part_number, etag, size_bytes, encrypted, encryption_key, key_id, plaintext_size, created_at
		 FROM multipart_parts
		 WHERE upload_id = ?
		 ORDER BY part_number`,
		uploadID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get parts: %w", err)
	}
	defer rows.Close()

	var parts []store.MultipartPart
	for rows.Next() {
		var (
			p         store.MultipartPart
			keyID     sql.NullString
			ptSize    sql.NullInt64
			createdAt string
		)
		if err := rows.Scan(&p.PartNumber, &p.ETag, &p.SizeBytes, &p.Encrypted, &p.EncryptionKey, &keyID, &ptSize, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan part: %w", err)
		}
		if keyID.Valid {
			p.KeyID = keyID.String
		}
		if ptSize.Valid {
			p.PlaintextSize = ptSize.Int64
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

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
func (s *Store) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]store.MultipartUpload, error) {
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
	defer rows.Close()

	var uploads []store.MultipartUpload
	for rows.Next() {
		var (
			mu          store.MultipartUpload
			contentType sql.NullString
			createdAt   string
		)
		if err := rows.Scan(&mu.UploadID, &mu.ObjectKey, &contentType, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan multipart upload: %w", err)
		}
		if contentType.Valid {
			mu.ContentType = contentType.String
		}
		mu.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		uploads = append(uploads, mu)
	}
	return uploads, rows.Err()
}

// CountActiveMultipartUploads returns the number of in-progress multipart
// uploads whose key starts with the given bucket prefix.
func (s *Store) CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error) {
	escapedPrefix := likeEscape(bucketPrefix)

	var count int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM multipart_uploads
		 WHERE object_key LIKE ? || '%' ESCAPE '\'`,
		escapedPrefix,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count active multipart uploads: %w", err)
	}
	return count, nil
}

// GetStaleMultipartUploads returns uploads older than the given duration.
func (s *Store) GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]store.MultipartUpload, error) {
	cutoff := time.Now().Add(-olderThan).UTC().Format(time.RFC3339Nano)

	rows, err := s.db.QueryContext(ctx,
		`SELECT upload_id, object_key, backend_name, content_type, metadata, created_at
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
func (s *Store) GetMultipartUploadsByBackend(ctx context.Context, backendName string) ([]store.MultipartUpload, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT upload_id, object_key, backend_name, content_type, metadata, created_at
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
	defer rows.Close()

	counts := make(map[string]int64)
	for rows.Next() {
		var backend string
		var count int64
		if err := rows.Scan(&backend, &count); err != nil {
			return nil, fmt.Errorf("failed to scan multipart count: %w", err)
		}
		counts[backend] = count
	}
	return counts, rows.Err()
}

// scanMultipartUploads converts sql.Rows into a slice of MultipartUpload.
func scanMultipartUploads(rows *sql.Rows) ([]store.MultipartUpload, error) {
	var uploads []store.MultipartUpload
	for rows.Next() {
		var (
			mu          store.MultipartUpload
			contentType sql.NullString
			metaJSON    sql.NullString
			createdAt   string
		)
		if err := rows.Scan(&mu.UploadID, &mu.ObjectKey, &mu.BackendName, &contentType, &metaJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan multipart upload: %w", err)
		}
		if contentType.Valid {
			mu.ContentType = contentType.String
		}
		if metaJSON.Valid && metaJSON.String != "" {
			if err := json.Unmarshal([]byte(metaJSON.String), &mu.Metadata); err != nil {
				return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
			}
		}
		mu.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		uploads = append(uploads, mu)
	}
	return uploads, rows.Err()
}

// likeEscape escapes SQL LIKE wildcards in prefix strings.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
