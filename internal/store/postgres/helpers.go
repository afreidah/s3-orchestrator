// -------------------------------------------------------------------------------
// Postgres - Type Translation Helpers
//
// Author: Alex Freidah
//
// Small helpers for translating between the canonical core domain types and
// the sqlc-generated row structs. Kept narrow because most translation
// happens in the file that owns the operation; these are the cross-file
// helpers used by the adapter.
// -------------------------------------------------------------------------------

package postgres

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// -------------------------------------------------------------------------
// NULLABLE CONVERSIONS
// -------------------------------------------------------------------------

// timestamptzPtr returns a *time.Time for a sqlc-emitted pgtype.Timestamptz,
// or nil when the column was NULL.
func timestamptzPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// changed reports whether an :execrows write touched a row, for the
// conditional writes whose outcome is the answer. what prefixes the failure.
func changed(n int64, err error, what string) (bool, error) {
	if err != nil {
		return false, fmt.Errorf("%s: %w", what, err)
	}
	return n > 0, nil
}

// strPtr returns a pointer to s when non-empty, nil otherwise.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// int64Ptr returns a pointer to n when non-zero, nil otherwise.
func int64Ptr(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}

// int16Ptr returns a pointer to n narrowed to int16 when non-zero, nil
// otherwise. The compression format version is a SMALLINT because it counts
// format revisions, and core carries it as an int like every other count.
func int16Ptr(n int) *int16 {
	if n == 0 {
		return nil
	}
	v := int16(n) //nolint:gosec // G115: format versions are single digits
	return &v
}

// derefOr returns *p when p is non-nil, otherwise zero. Replaces the
// `if p != nil { return *p } return zero` boilerplate at every nullable
// column read site so the caller is a single expression.
func derefOr[T any](p *T, zero T) T {
	if p == nil {
		return zero
	}
	return *p
}

// derefStr safely dereferences a nullable string pointer.
func derefStr(p *string) string { return derefOr(p, "") }

// derefInt64 safely dereferences a nullable int64 pointer.
func derefInt64(p *int64) int64 { return derefOr(p, 0) }

// derefInt16 safely dereferences a nullable int16 pointer.
func derefInt16(p *int16) int16 { return derefOr(p, 0) }

// -------------------------------------------------------------------------
// ROW MAPPING
// -------------------------------------------------------------------------

// mapSlice applies fn to each element of in, by pointer so large sqlc rows are
// not copied.
func mapSlice[I, O any](in []I, fn func(*I) O) []O {
	out := make([]O, len(in))
	for i := range in {
		out[i] = fn(&in[i])
	}
	return out
}

// totalsByBackend folds a per-backend result set into the map its caller
// returns, so each caller states only which columns carry the name and the
// total.
func totalsByBackend[T any](rows []T, split func(T) (string, int64)) map[string]int64 {
	out := make(map[string]int64, len(rows))
	for i := range rows {
		name, total := split(rows[i])
		out[name] = total
	}
	return out
}

// existingCopiesFromRows maps sqlc GetExistingCopiesForUpdate rows onto
// core.ExistingCopy values.
func existingCopiesFromRows(rows []db.GetExistingCopiesForUpdateRow) []core.ExistingCopy {
	return mapSlice(rows, existingCopyFromRow)
}

// existingCopyFromRow is the per-row converter used by
// existingCopiesFromRows.
func existingCopyFromRow(r *db.GetExistingCopiesForUpdateRow) core.ExistingCopy {
	return core.ExistingCopy{
		BackendName: r.BackendName,
		StorageKey:  r.StorageKey,
		SizeBytes:   r.SizeBytes,
		CreatedAt:   r.CreatedAt.Time,
		Form:        core.StoredFlags(r.Form),
	}
}

// -------------------------------------------------------------------------
// INSERT PARAMETERS
// -------------------------------------------------------------------------

// objectInsertParams maps a core.ObjectLocation onto the sqlc insert
// struct, attaching the representation metadata that is present. A zero
// CreatedAt passes NULL, which the query stamps as now.
func objectInsertParams(loc *core.ObjectLocation) db.InsertObjectLocationParams {
	form := loc.Recorded()
	params := db.InsertObjectLocationParams{
		ObjectKey:                loc.ObjectKey,
		BackendName:              loc.BackendName,
		StorageKey:               core.StoragePath(loc.ObjectKey, loc.StorageKey),
		SizeBytes:                loc.SizeBytes,
		Encrypted:                form.Encrypted,
		EncryptionKey:            form.EncryptionKey,
		KeyID:                    strPtr(form.KeyID),
		PlaintextSize:            int64Ptr(form.PlaintextSize),
		ContentHash:              strPtr(form.ContentHash),
		CompressionAlgorithm:     strPtr(form.CompressionAlgorithm),
		CompressionLevel:         strPtr(form.CompressionLevel),
		CompressionFormatVersion: int16Ptr(form.CompressionFormatVersion),
		LogicalSize:              int64Ptr(form.LogicalSize),
		CompressionProbeSize:     int64Ptr(loc.CompressionProbeSize),
		CompressionProbeLevel:    strPtr(loc.CompressionProbeLevel),
		CreatedAt:                pgtype.Timestamptz{Time: loc.CreatedAt, Valid: !loc.CreatedAt.IsZero()},
	}
	if id := loc.Identity; id != nil {
		params.Etag = strPtr(id.ETag)
		params.ContentType = strPtr(id.ContentType)
		// A marshal failure would mean a map[string]string that cannot be
		// JSON, which does not exist; the column stays NULL rather than
		// failing a write over metadata a later read can re-learn.
		params.UserMetadata, _ = core.EncodeUserMetadata(id.UserMetadata)
	}
	return params
}

// objectInsertIfNotExistsParams builds the conditional-insert params from the
// same mapping the unconditional insert uses, so the two can only ever write
// the same columns from the same location. They previously diverged, and the
// conditional one silently dropped every encryption field.
func objectInsertIfNotExistsParams(loc *core.ObjectLocation) db.InsertObjectLocationIfNotExistsParams {
	p := objectInsertParams(loc)
	return db.InsertObjectLocationIfNotExistsParams{
		ObjectKey:                p.ObjectKey,
		BackendName:              p.BackendName,
		StorageKey:               p.StorageKey,
		SizeBytes:                p.SizeBytes,
		Encrypted:                p.Encrypted,
		EncryptionKey:            p.EncryptionKey,
		KeyID:                    p.KeyID,
		PlaintextSize:            p.PlaintextSize,
		ContentHash:              p.ContentHash,
		CompressionAlgorithm:     p.CompressionAlgorithm,
		CompressionLevel:         p.CompressionLevel,
		CompressionFormatVersion: p.CompressionFormatVersion,
		LogicalSize:              p.LogicalSize,
		Etag:                     p.Etag,
		ContentType:              p.ContentType,
		UserMetadata:             p.UserMetadata,
		Managed:                  !loc.Unmanaged,
		CreatedAt:                p.CreatedAt,
	}
}
