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

// GetRandomHashedObjectsRow

func (r GetRandomHashedObjectsRow) GetObjectKey() string             { return r.ObjectKey }
func (r GetRandomHashedObjectsRow) GetBackendName() string           { return r.BackendName }
func (r GetRandomHashedObjectsRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r GetRandomHashedObjectsRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }
func (r GetRandomHashedObjectsRow) GetEncrypted() bool               { return r.Encrypted }
func (r GetRandomHashedObjectsRow) GetEncryptionKey() []byte         { return r.EncryptionKey }
func (r GetRandomHashedObjectsRow) GetKeyID() *string                { return r.KeyID }
func (r GetRandomHashedObjectsRow) GetPlaintextSize() *int64         { return r.PlaintextSize }
func (r GetRandomHashedObjectsRow) GetContentHash() *string          { return r.ContentHash }

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
