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

// derefStr safely dereferences a nullable string pointer.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// derefInt64 safely dereferences a nullable int64 pointer.
func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// existingCopiesFromRows maps sqlc GetExistingCopiesForUpdate rows onto
// core.ExistingCopy values.
func existingCopiesFromRows(rows []db.GetExistingCopiesForUpdateRow) []core.ExistingCopy {
	out := make([]core.ExistingCopy, len(rows))
	for i := range rows {
		out[i] = core.ExistingCopy{
			BackendName: rows[i].BackendName,
			SizeBytes:   rows[i].SizeBytes,
			CreatedAt:   rows[i].CreatedAt.Time,
		}
	}
	return out
}

// objectInsertParams maps a core.ObjectLocation onto the sqlc insert
// struct, attaching encryption + content-hash metadata when present.
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
	return params
}
