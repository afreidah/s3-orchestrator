// -------------------------------------------------------------------------------
// Object Location Accessors
//
// Author: Alex Freidah
//
// Hand-written companion to the sqlc-generated row types. Each row that is
// projected into store.ObjectLocation exposes a small accessor surface so the
// upper layer can convert any of the row shapes through a single generic
// helper instead of a type switch.
//
// sqlc only regenerates *.sql.go and the central models.go/db.go files, so
// this file survives `make generate`. When adding a new row type that
// projects ObjectLocation columns, add the corresponding accessor methods
// here and the conversion helper in internal/store/postgres/store.go picks it up
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
func (r ListObjectsByBackendRow) GetStorageKey() string            { return r.StorageKey }
func (r ListObjectsByBackendRow) GetBackendName() string           { return r.BackendName }
func (r ListObjectsByBackendRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r ListObjectsByBackendRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }

// ListObjectsByPrefixRow

func (r ListObjectsByPrefixRow) GetObjectKey() string             { return r.ObjectKey }
func (r ListObjectsByPrefixRow) GetStorageKey() string            { return r.StorageKey }
func (r ListObjectsByPrefixRow) GetBackendName() string           { return r.BackendName }
func (r ListObjectsByPrefixRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r ListObjectsByPrefixRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }

// ListExpiredObjectsRow

func (r ListExpiredObjectsRow) GetObjectKey() string             { return r.ObjectKey }
func (r ListExpiredObjectsRow) GetStorageKey() string            { return r.StorageKey }
func (r ListExpiredObjectsRow) GetBackendName() string           { return r.BackendName }
func (r ListExpiredObjectsRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r ListExpiredObjectsRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }

// ListDirectChildrenRow has no ObjectLocation projection; it groups by
// object_key and exposes a backend_names array, so it does not fit the
// single-backend ObjectLocation shape consumed by toObjectLocation.

// ListObjectsByBackendKeyAscRow

func (r ListObjectsByBackendKeyAscRow) GetObjectKey() string             { return r.ObjectKey }
func (r ListObjectsByBackendKeyAscRow) GetStorageKey() string            { return r.StorageKey }
func (r ListObjectsByBackendKeyAscRow) GetBackendName() string           { return r.BackendName }
func (r ListObjectsByBackendKeyAscRow) GetSizeBytes() int64              { return r.SizeBytes }
func (r ListObjectsByBackendKeyAscRow) GetCreatedAt() pgtype.Timestamptz { return r.CreatedAt }
