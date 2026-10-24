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
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// -------------------------------------------------------------------------
// UPLOAD LIFECYCLE
// -------------------------------------------------------------------------

// CreateMultipartUpload records a new multipart upload in the database, unless
// its backend is being drained, which reports false. Encryption fields on params
// are persisted so every UploadPart against the row can reuse the same wrapped
// DEK; they are zero-length when proxy-side encryption is disabled.
func (s *Store) CreateMultipartUpload(ctx context.Context, params *core.CreateMultipartUploadParams) (bool, error) {
	var metaJSON []byte
	if len(params.Metadata) > 0 {
		var err error
		metaJSON, err = json.Marshal(params.Metadata)
		if err != nil {
			return false, fmt.Errorf("failed to marshal metadata: %w", err)
		}
	}
	keyIDPtr := strPtr(params.KeyID)
	contentTypePtr := strPtr(params.ContentType)
	n, err := s.queries.CreateMultipartUpload(ctx, db.CreateMultipartUploadParams{
		UploadID:      params.UploadID,
		ObjectKey:     params.ObjectKey,
		BackendName:   params.BackendName,
		ContentType:   contentTypePtr,
		Metadata:      metaJSON,
		EncryptionKey: nilIfEmptyBytes(params.EncryptionKey),
		KeyID:         keyIDPtr,
		Tagging:       strPtr(core.EncodeTags(params.Tags)),
		Accepts:       int64(core.AcceptForWrite),
	})
	return changed(n, err, "failed to create multipart upload")
}

// BeginMultipartCompletion marks the upload as being completed, reporting
// false when a completion that renewed its mark after staleBefore holds it or
// the upload is gone.
func (s *Store) BeginMultipartCompletion(ctx context.Context, uploadID string, staleBefore time.Time) (bool, error) {
	n, err := s.queries.BeginMultipartCompletion(ctx, db.BeginMultipartCompletionParams{
		UploadID:    uploadID,
		StaleBefore: pgTimestamptz(staleBefore),
	})
	return changed(n, err, "failed to begin multipart completion")
}

// RenewMultipartCompletion refreshes the upload's completion mark, reporting
// false once the mark or the upload is gone.
func (s *Store) RenewMultipartCompletion(ctx context.Context, uploadID string) (bool, error) {
	n, err := s.queries.RenewMultipartCompletion(ctx, uploadID)
	return changed(n, err, "failed to renew multipart completion")
}

// EndMultipartCompletion clears the upload's completion mark, so a completion
// that failed leaves the upload open to parts, aborts and a retry.
func (s *Store) EndMultipartCompletion(ctx context.Context, uploadID string) error {
	if err := s.queries.EndMultipartCompletion(ctx, uploadID); err != nil {
		return fmt.Errorf("failed to end multipart completion: %w", err)
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
	if mu.Tags, err = core.DecodeTags(derefStr(row.Tagging)); err != nil {
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
	meta, err := core.DecodeUserMetadata(r.Metadata)
	if err != nil {
		return core.MultipartUpload{}, err
	}
	return core.MultipartUpload{
		UploadID:      r.UploadID,
		ObjectKey:     r.ObjectKey,
		BackendName:   r.BackendName,
		ContentType:   derefStr(r.ContentType),
		Metadata:      meta,
		EncryptionKey: r.EncryptionKey,
		KeyID:         derefStr(r.KeyID),
		Encrypted:     len(r.EncryptionKey) > 0,
		CreatedAt:     r.CreatedAt,
	}, nil
}

// -------------------------------------------------------------------------
// PARTS
// -------------------------------------------------------------------------

// ListParts returns up to limit parts numbered above afterPart, ordered by
// part number.
func (s *Store) ListParts(ctx context.Context, uploadID string, afterPart, limit int) ([]core.MultipartPart, error) {
	rows, err := s.queries.ListParts(ctx, db.ListPartsParams{
		UploadID:  uploadID,
		AfterPart: int32(afterPart), //nolint:gosec // G115: the S3 handler parses the marker as a 32-bit integer
		RowLimit:  rowLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list parts: %w", err)
	}
	return mapSlice(rows, multipartPartFromRow), nil
}

// multipartPartFromRow converts a sqlc ListParts row into the canonical
// core.MultipartPart shape, safely dereferencing nullable KeyID and
// PlaintextSize.
func multipartPartFromRow(r *db.ListPartsRow) core.MultipartPart {
	return core.MultipartPart{
		Encrypted:     r.Encrypted,
		EncryptionKey: r.EncryptionKey,
		KeyID:         derefStr(r.KeyID),
		PlaintextSize: derefInt64(r.PlaintextSize),
		PartNumber:    int(r.PartNumber),
		ETag:          r.Etag,
		PlaintextETag: derefStr(r.PlaintextEtag),
		SizeBytes:     r.SizeBytes,
		StorageKey:    derefStr(r.StorageKey),
		CreatedAt:     r.CreatedAt.Time,
	}
}

// -------------------------------------------------------------------------
// DELETION, LISTING, COUNTS
// -------------------------------------------------------------------------

// collectMultipartUploads is the shared row->core.MultipartUpload
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

// ScanMultipartUploads returns up to limit uploads matching filter after
// afterUploadID, in upload_id order, for the abort scans.
func (s *Store) ScanMultipartUploads(ctx context.Context, filter core.MultipartUploadFilter, limit int, afterUploadID string) ([]core.MultipartUpload, error) {
	var cutoff pgtype.Timestamptz
	if !filter.CreatedBefore.IsZero() {
		cutoff = pgTimestamptz(filter.CreatedBefore)
	}
	rows, err := s.queries.ScanMultipartUploads(ctx, db.ScanMultipartUploadsParams{
		BackendFilter: filter.Backend,
		CreatedBefore: cutoff,
		AfterUploadID: afterUploadID,
		RowLimit:      rowLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to scan multipart uploads: %w", err)
	}
	return collectMultipartUploads(rows, func(r *db.ScanMultipartUploadsRow) *multipartRow {
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
		MaxUploads: rowLimit(maxUploads),
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
