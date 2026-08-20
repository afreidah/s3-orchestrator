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
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

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

// mapSlice applies fn to every element of in and returns the resulting
// slice. fn receives a pointer to each element so large sqlc row structs
// are not copied per call. The output slice is always pre-sized to
// len(in) so the loop body never reallocates. Replaces the
// `out := make([]T, len(rows)); for i,r := range rows { out[i] = toT(r) }`
// pattern at every sqlc-row-to-core conversion site.
func mapSlice[I, O any](in []I, fn func(*I) O) []O {
	out := make([]O, len(in))
	for i := range in {
		out[i] = fn(&in[i])
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
		SizeBytes:   r.SizeBytes,
		CreatedAt:   r.CreatedAt.Time,
		Encrypted:   r.Encrypted,
		HasDEK:      r.HasDek != nil && *r.HasDek,
	}
}

// objectInsertParams maps a core.ObjectLocation onto the sqlc insert
// struct, attaching the representation metadata that is present.
func objectInsertParams(loc *core.ObjectLocation) db.InsertObjectLocationParams {
	params := db.InsertObjectLocationParams{
		ObjectKey:   loc.ObjectKey,
		BackendName: loc.BackendName,
		SizeBytes:   loc.SizeBytes,
	}
	if loc.Encrypted {
		params.Encrypted = true
		params.EncryptionKey = loc.EncryptionKey
		params.KeyID = strPtr(loc.KeyID)
		params.PlaintextSize = int64Ptr(loc.PlaintextSize)
	}
	if loc.ContentHash != "" {
		params.ContentHash = strPtr(loc.ContentHash)
	}
	if loc.CompressionAlgorithm != "" {
		params.CompressionAlgorithm = strPtr(loc.CompressionAlgorithm)
		params.CompressionLevel = strPtr(loc.CompressionLevel)
		params.CompressionFormatVersion = int16Ptr(loc.CompressionFormatVersion)
		params.LogicalSize = int64Ptr(loc.LogicalSize)
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
		Managed:                  !loc.Unmanaged,
	}
}
