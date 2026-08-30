// -------------------------------------------------------------------------------
// Object Location Accessors
//
// Author: Alex Freidah
//
// Hand-written companion to the sqlc-generated row types. Each row that is
// projected into store.ObjectLocation exposes a small accessor surface so the
// upper layer can convert any of the eleven row shapes through a single
// generic helper instead of an eleven-case type switch.
//
// sqlc only regenerates *.sql.go and the central models.go/db.go files, so
// this file survives `make generate`. When adding a new row type that
// projects ObjectLocation columns, add the corresponding accessor methods
// here and the conversion helper in internal/store/store.go picks it up
// without further plumbing.
// -------------------------------------------------------------------------------

// Package db contains the sqlc-generated query bindings for the
// PostgreSQL engine plus a small handful of hand-written accessor
// helpers that share the package. Generated files are produced from
// internal/store/postgres/sqlc/queries; do not hand-edit those.
package db

import "github.com/jackc/pgx/v5/pgtype"

// -------------------------------------------------------------------------
// Slim rows (key, backend, size, created_at).
// -------------------------------------------------------------------------

// ListObjectsByBackendRow

func (r ListObjectsByBackendRow) GetObjectKey() string             { return r.ObjectKey }
func (r ListObjectsByBackendRow) GetBackendName() string           { return r.BackendName }
func (r ListObjectsByBackendRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r ListObjectsByBackendRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }

// ListObjectsByPrefixRow

func (r ListObjectsByPrefixRow) GetObjectKey() string             { return r.ObjectKey }
func (r ListObjectsByPrefixRow) GetBackendName() string           { return r.BackendName }
func (r ListObjectsByPrefixRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r ListObjectsByPrefixRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }

// ListExpiredObjectsRow

func (r ListExpiredObjectsRow) GetObjectKey() string             { return r.ObjectKey }
func (r ListExpiredObjectsRow) GetBackendName() string           { return r.BackendName }
func (r ListExpiredObjectsRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r ListExpiredObjectsRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }

// ListDirectChildrenRow has no ObjectLocation projection; it groups by
// object_key and exposes a backend_names array, so it does not fit the
// single-backend ObjectLocation shape consumed by toObjectLocation.

// ListObjectsByBackendKeyAscRow

func (r ListObjectsByBackendKeyAscRow) GetObjectKey() string             { return r.ObjectKey }
func (r ListObjectsByBackendKeyAscRow) GetBackendName() string           { return r.BackendName }
func (r ListObjectsByBackendKeyAscRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r ListObjectsByBackendKeyAscRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }

// -------------------------------------------------------------------------
// Fat rows (slim columns + encryption + content hash).
// -------------------------------------------------------------------------

// GetAllObjectLocationsRow

func (r GetAllObjectLocationsRow) GetObjectKey() string             { return r.ObjectKey }
func (r GetAllObjectLocationsRow) GetBackendName() string           { return r.BackendName }
func (r GetAllObjectLocationsRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r GetAllObjectLocationsRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }
func (r GetAllObjectLocationsRow) GetEncrypted() bool               { return r.Encrypted }
func (r GetAllObjectLocationsRow) GetEncryptionKey() []byte         { return r.EncryptionKey }
func (r GetAllObjectLocationsRow) GetKeyID() *string                { return r.KeyID }
func (r GetAllObjectLocationsRow) GetPlaintextSize() *int64         { return r.PlaintextSize }
func (r GetAllObjectLocationsRow) GetContentHash() *string          { return r.ContentHash }
func (r GetAllObjectLocationsRow) GetCompressionAlgorithm() *string { return r.CompressionAlgorithm }
func (r GetAllObjectLocationsRow) GetCompressionLevel() *string     { return r.CompressionLevel }
func (r GetAllObjectLocationsRow) GetLogicalSize() *int64           { return r.LogicalSize }
func (r GetAllObjectLocationsRow) GetCompressionFormatVersion() *int16 {
	return r.CompressionFormatVersion
}
func (r GetAllObjectLocationsRow) GetLastScrubbedAt() pgtype.Timestamptz {
	return r.LastScrubbedAt
}
func (r GetAllObjectLocationsRow) GetEtag() *string        { return r.Etag }
func (r GetAllObjectLocationsRow) GetContentType() *string { return r.ContentType }
func (r GetAllObjectLocationsRow) GetUserMetadata() []byte { return r.UserMetadata }

// GetUnderReplicatedObjectsRow

func (r GetUnderReplicatedObjectsRow) GetObjectKey() string             { return r.ObjectKey }
func (r GetUnderReplicatedObjectsRow) GetBackendName() string           { return r.BackendName }
func (r GetUnderReplicatedObjectsRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r GetUnderReplicatedObjectsRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }
func (r GetUnderReplicatedObjectsRow) GetEncrypted() bool               { return r.Encrypted }
func (r GetUnderReplicatedObjectsRow) GetEncryptionKey() []byte         { return r.EncryptionKey }
func (r GetUnderReplicatedObjectsRow) GetKeyID() *string                { return r.KeyID }
func (r GetUnderReplicatedObjectsRow) GetPlaintextSize() *int64         { return r.PlaintextSize }
func (r GetUnderReplicatedObjectsRow) GetContentHash() *string          { return r.ContentHash }
func (r GetUnderReplicatedObjectsRow) GetCompressionLevel() *string     { return r.CompressionLevel }
func (r GetUnderReplicatedObjectsRow) GetLogicalSize() *int64           { return r.LogicalSize }
func (r GetUnderReplicatedObjectsRow) GetCompressionAlgorithm() *string {
	return r.CompressionAlgorithm
}
func (r GetUnderReplicatedObjectsRow) GetCompressionFormatVersion() *int16 {
	return r.CompressionFormatVersion
}

// GetUnderReplicatedObjectsExcludingRow

func (r GetUnderReplicatedObjectsExcludingRow) GetObjectKey() string             { return r.ObjectKey }
func (r GetUnderReplicatedObjectsExcludingRow) GetBackendName() string           { return r.BackendName }
func (r GetUnderReplicatedObjectsExcludingRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r GetUnderReplicatedObjectsExcludingRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }
func (r GetUnderReplicatedObjectsExcludingRow) GetEncrypted() bool               { return r.Encrypted }
func (r GetUnderReplicatedObjectsExcludingRow) GetEncryptionKey() []byte         { return r.EncryptionKey }
func (r GetUnderReplicatedObjectsExcludingRow) GetKeyID() *string                { return r.KeyID }
func (r GetUnderReplicatedObjectsExcludingRow) GetPlaintextSize() *int64         { return r.PlaintextSize }
func (r GetUnderReplicatedObjectsExcludingRow) GetContentHash() *string          { return r.ContentHash }
func (r GetUnderReplicatedObjectsExcludingRow) GetCompressionLevel() *string {
	return r.CompressionLevel
}
func (r GetUnderReplicatedObjectsExcludingRow) GetLogicalSize() *int64 { return r.LogicalSize }
func (r GetUnderReplicatedObjectsExcludingRow) GetCompressionAlgorithm() *string {
	return r.CompressionAlgorithm
}
func (r GetUnderReplicatedObjectsExcludingRow) GetCompressionFormatVersion() *int16 {
	return r.CompressionFormatVersion
}

// GetOverReplicatedObjectsRow

func (r GetOverReplicatedObjectsRow) GetObjectKey() string             { return r.ObjectKey }
func (r GetOverReplicatedObjectsRow) GetBackendName() string           { return r.BackendName }
func (r GetOverReplicatedObjectsRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r GetOverReplicatedObjectsRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }
func (r GetOverReplicatedObjectsRow) GetEncrypted() bool               { return r.Encrypted }
func (r GetOverReplicatedObjectsRow) GetEncryptionKey() []byte         { return r.EncryptionKey }
func (r GetOverReplicatedObjectsRow) GetKeyID() *string                { return r.KeyID }
func (r GetOverReplicatedObjectsRow) GetPlaintextSize() *int64         { return r.PlaintextSize }
func (r GetOverReplicatedObjectsRow) GetContentHash() *string          { return r.ContentHash }
func (r GetOverReplicatedObjectsRow) GetCompressionAlgorithm() *string { return r.CompressionAlgorithm }
func (r GetOverReplicatedObjectsRow) GetCompressionLevel() *string     { return r.CompressionLevel }
func (r GetOverReplicatedObjectsRow) GetLogicalSize() *int64           { return r.LogicalSize }
func (r GetOverReplicatedObjectsRow) GetCompressionFormatVersion() *int16 {
	return r.CompressionFormatVersion
}

// GetLeastRecentlyScrubbedObjectsRow

func (r GetLeastRecentlyScrubbedObjectsRow) GetObjectKey() string             { return r.ObjectKey }
func (r GetLeastRecentlyScrubbedObjectsRow) GetBackendName() string           { return r.BackendName }
func (r GetLeastRecentlyScrubbedObjectsRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r GetLeastRecentlyScrubbedObjectsRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }
func (r GetLeastRecentlyScrubbedObjectsRow) GetEncrypted() bool               { return r.Encrypted }
func (r GetLeastRecentlyScrubbedObjectsRow) GetEncryptionKey() []byte         { return r.EncryptionKey }
func (r GetLeastRecentlyScrubbedObjectsRow) GetKeyID() *string                { return r.KeyID }
func (r GetLeastRecentlyScrubbedObjectsRow) GetPlaintextSize() *int64         { return r.PlaintextSize }
func (r GetLeastRecentlyScrubbedObjectsRow) GetContentHash() *string          { return r.ContentHash }
func (r GetLeastRecentlyScrubbedObjectsRow) GetCompressionLevel() *string     { return r.CompressionLevel }
func (r GetLeastRecentlyScrubbedObjectsRow) GetLogicalSize() *int64           { return r.LogicalSize }
func (r GetLeastRecentlyScrubbedObjectsRow) GetCompressionAlgorithm() *string {
	return r.CompressionAlgorithm
}
func (r GetLeastRecentlyScrubbedObjectsRow) GetCompressionFormatVersion() *int16 {
	return r.CompressionFormatVersion
}
func (r GetLeastRecentlyScrubbedObjectsRow) GetLastScrubbedAt() pgtype.Timestamptz {
	return r.LastScrubbedAt
}

// GetObjectsWithoutHashRow

func (r GetObjectsWithoutHashRow) GetObjectKey() string             { return r.ObjectKey }
func (r GetObjectsWithoutHashRow) GetBackendName() string           { return r.BackendName }
func (r GetObjectsWithoutHashRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r GetObjectsWithoutHashRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }
func (r GetObjectsWithoutHashRow) GetEncrypted() bool               { return r.Encrypted }
func (r GetObjectsWithoutHashRow) GetEncryptionKey() []byte         { return r.EncryptionKey }
func (r GetObjectsWithoutHashRow) GetKeyID() *string                { return r.KeyID }
func (r GetObjectsWithoutHashRow) GetPlaintextSize() *int64         { return r.PlaintextSize }
func (r GetObjectsWithoutHashRow) GetContentHash() *string          { return r.ContentHash }
func (r GetObjectsWithoutHashRow) GetCompressionAlgorithm() *string { return r.CompressionAlgorithm }
func (r GetObjectsWithoutHashRow) GetCompressionLevel() *string     { return r.CompressionLevel }
func (r GetObjectsWithoutHashRow) GetLogicalSize() *int64           { return r.LogicalSize }
func (r GetObjectsWithoutHashRow) GetCompressionFormatVersion() *int16 {
	return r.CompressionFormatVersion
}
