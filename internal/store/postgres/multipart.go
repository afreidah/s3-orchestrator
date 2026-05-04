// -------------------------------------------------------------------------------
// Multipart Upload Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for the in-progress multipart
// upload state stored in multipart_uploads + multipart_parts. Carries
// upload create/lookup/delete, per-part record/list/delete, and the
// stale-upload sweep used by the multipart cleanup background worker.
// The metadata JSONB column lets clients pass arbitrary user metadata
// through CompleteMultipartUpload without a schema change.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// errUnmarshalMetadata is the wrap format string used by every site
// that unmarshals the JSONB metadata column. Centralised so the audit
// log "failed to unmarshal metadata" string stays grep-able and a typo
// in one site doesn't drift from the others.
const errUnmarshalMetadata = "failed to unmarshal metadata: %w"

// -------------------------------------------------------------------------
// UPLOAD LIFECYCLE
// -------------------------------------------------------------------------

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
	err := s.queries.CreateMultipartUpload(ctx, db.CreateMultipartUploadParams{
		UploadID:    uploadID,
		ObjectKey:   key,
		BackendName: backend,
		ContentType: &contentType,
		Metadata:    metaJSON,
	})
	if err != nil {
		return fmt.Errorf("failed to create multipart upload: %w", err)
	}
	return nil
}

// GetMultipartUpload retrieves metadata for a multipart upload.
func (s *Store) GetMultipartUpload(ctx context.Context, uploadID string) (*core.MultipartUpload, error) {
	row, err := s.queries.GetMultipartUpload(ctx, uploadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, core.ErrMultipartUploadNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get multipart upload: %w", err)
	}
	mu, err := toMultipartUpload(row.UploadID, row.ObjectKey, row.BackendName, row.ContentType, row.Metadata, row.CreatedAt.Time)
	if err != nil {
		return nil, err
	}
	return &mu, nil
}

// toMultipartUpload assembles a MultipartUpload from the column set every
// sqlc multipart row exposes. Centralizes the pointer-deref of ContentType
// and the JSON unmarshal of Metadata so the slice-returning callers above
// and below this don't carry parallel loop bodies.
func toMultipartUpload(uploadID, objectKey, backendName string, contentType *string, metadata []byte, createdAt time.Time) (core.MultipartUpload, error) {
	ct := ""
	if contentType != nil {
		ct = *contentType
	}
	mu := core.MultipartUpload{
		UploadID:    uploadID,
		ObjectKey:   objectKey,
		BackendName: backendName,
		ContentType: ct,
		CreatedAt:   createdAt,
	}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &mu.Metadata); err != nil {
			return core.MultipartUpload{}, fmt.Errorf(errUnmarshalMetadata, err)
		}
	}
	return mu, nil
}

// -------------------------------------------------------------------------
// PARTS
// -------------------------------------------------------------------------

// RecordPart records a completed part for a multipart upload.
// S3 spec requires part numbers between 1 and 10000.
func (s *Store) RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, enc *core.EncryptionMeta) error {
	if partNumber < 1 || partNumber > 10000 {
		return fmt.Errorf("invalid part number %d: must be between 1 and 10000", partNumber)
	}
	params := db.UpsertPartParams{
		UploadID:   uploadID,
		PartNumber: int32(partNumber),
		Etag:       etag,
		SizeBytes:  size,
	}
	if enc != nil && enc.Encrypted {
		params.Encrypted = true
		params.EncryptionKey = enc.EncryptionKey
		params.KeyID = &enc.KeyID
		params.PlaintextSize = &enc.PlaintextSize
	}
	if err := s.queries.UpsertPart(ctx, params); err != nil {
		return fmt.Errorf("failed to record part: %w", err)
	}
	return nil
}

// GetParts returns all parts for a multipart upload, ordered by part number.
func (s *Store) GetParts(ctx context.Context, uploadID string) ([]core.MultipartPart, error) {
	rows, err := s.queries.GetParts(ctx, uploadID)
	if err != nil {
		return nil, fmt.Errorf("failed to get parts: %w", err)
	}

	parts := make([]core.MultipartPart, len(rows))
	for i, row := range rows {
		p := core.MultipartPart{
			PartNumber:    int(row.PartNumber),
			ETag:          row.Etag,
			SizeBytes:     row.SizeBytes,
			CreatedAt:     row.CreatedAt.Time,
			Encrypted:     row.Encrypted,
			EncryptionKey: row.EncryptionKey,
		}
		if row.KeyID != nil {
			p.KeyID = *row.KeyID
		}
		if row.PlaintextSize != nil {
			p.PlaintextSize = *row.PlaintextSize
		}
		parts[i] = p
	}
	return parts, nil
}

// -------------------------------------------------------------------------
// DELETION, LISTING, COUNTS
// -------------------------------------------------------------------------

// DeleteMultipartUpload removes a multipart upload and its parts (cascading).
func (s *Store) DeleteMultipartUpload(ctx context.Context, uploadID string) error {
	err := s.queries.DeleteMultipartUpload(ctx, uploadID)
	if err != nil {
		return fmt.Errorf("failed to delete multipart upload: %w", err)
	}
	return nil
}

// GetStaleMultipartUploads returns uploads older than the given duration.
func (s *Store) GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]core.MultipartUpload, error) {
	cutoff := time.Now().Add(-olderThan)
	rows, err := s.queries.GetStaleMultipartUploads(ctx, pgTimestamptz(cutoff))
	if err != nil {
		return nil, fmt.Errorf("failed to get stale uploads: %w", err)
	}
	uploads := make([]core.MultipartUpload, len(rows))
	for i, row := range rows {
		mu, err := toMultipartUpload(row.UploadID, row.ObjectKey, row.BackendName, row.ContentType, row.Metadata, row.CreatedAt.Time)
		if err != nil {
			return nil, err
		}
		uploads[i] = mu
	}
	return uploads, nil
}

// GetMultipartUploadsByBackend returns all in-progress multipart uploads on
// the given backend. Used by drain to abort uploads before migrating objects.
// Requires live PostgreSQL  -  covered by integration tests.
func (s *Store) GetMultipartUploadsByBackend(ctx context.Context, backendName string) ([]core.MultipartUpload, error) {
	rows, err := s.queries.GetMultipartUploadsByBackend(ctx, backendName)
	if err != nil {
		return nil, fmt.Errorf("failed to get multipart uploads by backend: %w", err)
	}
	uploads := make([]core.MultipartUpload, len(rows))
	for i, row := range rows {
		mu, err := toMultipartUpload(row.UploadID, row.ObjectKey, row.BackendName, row.ContentType, row.Metadata, row.CreatedAt.Time)
		if err != nil {
			return nil, err
		}
		uploads[i] = mu
	}
	return uploads, nil
}

// CountActiveMultipartUploads returns the number of in-progress multipart
// uploads whose key starts with the given bucket prefix.
func (s *Store) CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error) {
	escapedPrefix := likeEscaper.Replace(bucketPrefix)
	count, err := s.queries.CountActiveMultipartUploadsByPrefix(ctx, &escapedPrefix)
	if err != nil {
		return 0, fmt.Errorf("failed to count active multipart uploads: %w", err)
	}
	return count, nil
}

// ListMultipartUploads returns in-progress multipart uploads whose key matches
// the given prefix, up to maxUploads entries.
func (s *Store) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]core.MultipartUpload, error) {
	escapedPrefix := likeEscaper.Replace(prefix)

	rows, err := s.queries.ListMultipartUploadsByPrefix(ctx, db.ListMultipartUploadsByPrefixParams{
		Prefix:     &escapedPrefix,
		MaxUploads: int32(maxUploads), //nolint:gosec // G115: maxUploads is a small caller-controlled value
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list multipart uploads: %w", err)
	}

	uploads := make([]core.MultipartUpload, len(rows))
	for i, row := range rows {
		ct := ""
		if row.ContentType != nil {
			ct = *row.ContentType
		}
		uploads[i] = core.MultipartUpload{
			UploadID:    row.UploadID,
			ObjectKey:   row.ObjectKey,
			ContentType: ct,
			CreatedAt:   row.CreatedAt.Time,
		}
	}
	return uploads, nil
}
