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
// Encryption fields on params are persisted so every UploadPart against
// the row can reuse the same wrapped DEK; they are zero-length when
// proxy-side encryption is disabled.
func (s *Store) CreateMultipartUpload(ctx context.Context, params *core.CreateMultipartUploadParams) error {
	var metaJSON []byte
	if len(params.Metadata) > 0 {
		var err error
		metaJSON, err = json.Marshal(params.Metadata)
		if err != nil {
			return fmt.Errorf("failed to marshal metadata: %w", err)
		}
	}
	keyIDPtr := strPtr(params.KeyID)
	contentTypePtr := strPtr(params.ContentType)
	err := s.queries.CreateMultipartUpload(ctx, db.CreateMultipartUploadParams{
		UploadID:      params.UploadID,
		ObjectKey:     params.ObjectKey,
		BackendName:   params.BackendName,
		ContentType:   contentTypePtr,
		Metadata:      metaJSON,
		EncryptionKey: nilIfEmptyBytes(params.EncryptionKey),
		KeyID:         keyIDPtr,
	})
	if err != nil {
		return fmt.Errorf("failed to create multipart upload: %w", err)
	}
	return nil
}

// nilIfEmptyBytes returns nil when b is zero-length so the column lands
// as NULL rather than an empty BYTEA.
func nilIfEmptyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
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
	mu, err := toMultipartUpload(&multipartRow{
		UploadID:      row.UploadID,
		ObjectKey:     row.ObjectKey,
		BackendName:   row.BackendName,
		ContentType:   row.ContentType,
		Metadata:      row.Metadata,
		EncryptionKey: row.EncryptionKey,
		KeyID:         row.KeyID,
		CreatedAt:     row.CreatedAt.Time,
	})
	if err != nil {
		return nil, err
	}
	return &mu, nil
}

// multipartRow mirrors the column set every multipart_uploads sqlc row
// exposes. Used to feed toMultipartUpload from each query without
// duplicating the field-by-field copy at every call site.
type multipartRow struct {
	UploadID      string
	ObjectKey     string
	BackendName   string
	ContentType   *string
	Metadata      []byte
	EncryptionKey []byte
	KeyID         *string
	CreatedAt     time.Time
}

// toMultipartUpload assembles a MultipartUpload from a multipartRow.
// Centralizes the pointer-deref of ContentType/KeyID and the JSON
// unmarshal of Metadata so the slice-returning callers do not carry
// parallel loop bodies.
func toMultipartUpload(r *multipartRow) (core.MultipartUpload, error) {
	ct := ""
	if r.ContentType != nil {
		ct = *r.ContentType
	}
	kid := ""
	if r.KeyID != nil {
		kid = *r.KeyID
	}
	mu := core.MultipartUpload{
		UploadID:      r.UploadID,
		ObjectKey:     r.ObjectKey,
		BackendName:   r.BackendName,
		ContentType:   ct,
		EncryptionKey: r.EncryptionKey,
		KeyID:         kid,
		Encrypted:     len(r.EncryptionKey) > 0,
		CreatedAt:     r.CreatedAt,
	}
	if len(r.Metadata) > 0 {
		if err := json.Unmarshal(r.Metadata, &mu.Metadata); err != nil {
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

	return mapSlice(rows, multipartPartFromRow), nil
}

// multipartPartFromRow converts a sqlc GetParts row into the canonical
// core.MultipartPart shape, safely dereferencing nullable KeyID and
// PlaintextSize.
func multipartPartFromRow(r *db.GetPartsRow) core.MultipartPart {
	return core.MultipartPart{
		PartNumber:    int(r.PartNumber),
		ETag:          r.Etag,
		SizeBytes:     r.SizeBytes,
		CreatedAt:     r.CreatedAt.Time,
		Encrypted:     r.Encrypted,
		EncryptionKey: r.EncryptionKey,
		KeyID:         derefStr(r.KeyID),
		PlaintextSize: derefInt64(r.PlaintextSize),
	}
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

// collectMultipartUploads is the shared row→core.MultipartUpload
// pipeline used by every slice-returning multipart query. Each
// caller passes a per-row converter; the loop wiring + error
// handling lives here so the per-query bodies stay one-liners.
func collectMultipartUploads[T any](rows []T, conv func(*T) *multipartRow) ([]core.MultipartUpload, error) {
	uploads := make([]core.MultipartUpload, len(rows))
	for i := range rows {
		mu, err := toMultipartUpload(conv(&rows[i]))
		if err != nil {
			return nil, err
		}
		uploads[i] = mu
	}
	return uploads, nil
}

// GetStaleMultipartUploads returns uploads older than the given duration.
func (s *Store) GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]core.MultipartUpload, error) {
	cutoff := time.Now().Add(-olderThan)
	rows, err := s.queries.GetStaleMultipartUploads(ctx, pgTimestamptz(cutoff))
	if err != nil {
		return nil, fmt.Errorf("failed to get stale uploads: %w", err)
	}
	return collectMultipartUploads(rows, func(r *db.GetStaleMultipartUploadsRow) *multipartRow {
		return &multipartRow{
			UploadID: r.UploadID, ObjectKey: r.ObjectKey, BackendName: r.BackendName,
			ContentType: r.ContentType, Metadata: r.Metadata,
			EncryptionKey: r.EncryptionKey, KeyID: r.KeyID, CreatedAt: r.CreatedAt.Time,
		}
	})
}

// GetMultipartUploadsByBackend returns all in-progress multipart uploads on
// the given backend. Used by drain to abort uploads before migrating objects.
// Requires live PostgreSQL  -  covered by integration tests.
func (s *Store) GetMultipartUploadsByBackend(ctx context.Context, backendName string) ([]core.MultipartUpload, error) {
	rows, err := s.queries.GetMultipartUploadsByBackend(ctx, backendName)
	if err != nil {
		return nil, fmt.Errorf("failed to get multipart uploads by backend: %w", err)
	}
	return collectMultipartUploads(rows, func(r *db.GetMultipartUploadsByBackendRow) *multipartRow {
		return &multipartRow{
			UploadID: r.UploadID, ObjectKey: r.ObjectKey, BackendName: r.BackendName,
			ContentType: r.ContentType, Metadata: r.Metadata,
			EncryptionKey: r.EncryptionKey, KeyID: r.KeyID, CreatedAt: r.CreatedAt.Time,
		}
	})
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

	return mapSlice(rows, listedMultipartUploadFromRow), nil
}

// listedMultipartUploadFromRow converts a sqlc ListMultipartUploadsByPrefix
// row to the slimmer core.MultipartUpload shape returned to the listing
// caller (encryption metadata is omitted by design).
func listedMultipartUploadFromRow(r *db.ListMultipartUploadsByPrefixRow) core.MultipartUpload {
	return core.MultipartUpload{
		UploadID:    r.UploadID,
		ObjectKey:   r.ObjectKey,
		ContentType: derefStr(r.ContentType),
		CreatedAt:   r.CreatedAt.Time,
	}
}
