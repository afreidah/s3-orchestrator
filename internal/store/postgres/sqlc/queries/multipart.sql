-- -----------------------------------------------------------------------------
-- Multipart Upload Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for multipart_uploads and multipart_parts. Covers
-- the upload lifecycle (create, lookup, delete), per-part record/list, the
-- prefix-scoped listing the S3 ListMultipartUploads handler needs, the
-- stale-upload sweep used by the multipart cleanup background worker, and the
-- backfill helpers (ListLegacy*, UpdateUploadEncryption, UpdatePartEncryption)
-- the multipart_dek_backfill worker calls when promoting legacy rows to the
-- shared-DEK format introduced in migration 00010.
-- -----------------------------------------------------------------------------

-- name: CreateMultipartUpload :exec
INSERT INTO multipart_uploads (upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, NOW());

-- name: GetMultipartUpload :one
SELECT upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, created_at
FROM multipart_uploads
WHERE upload_id = $1;

-- name: UpsertPart :exec
INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes, encrypted, encryption_key, key_id, plaintext_size, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
ON CONFLICT (upload_id, part_number) DO UPDATE SET
    etag = $3, size_bytes = $4, encrypted = $5, encryption_key = $6, key_id = $7, plaintext_size = $8, created_at = NOW();

-- name: GetParts :many
SELECT part_number, etag, size_bytes, encrypted, encryption_key, key_id, plaintext_size, created_at
FROM multipart_parts
WHERE upload_id = $1
ORDER BY part_number;

-- name: DeleteMultipartUpload :exec
DELETE FROM multipart_uploads
WHERE upload_id = $1;

-- name: GetStaleMultipartUploads :many
SELECT upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, created_at
FROM multipart_uploads
WHERE created_at < $1;

-- name: GetMultipartUploadsByBackend :many
SELECT upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, created_at
FROM multipart_uploads
WHERE backend_name = $1;

-- name: DeleteMultipartUploadsByBackend :exec
DELETE FROM multipart_uploads WHERE backend_name = $1;

-- name: CountActiveMultipartUploadsByPrefix :one
SELECT COUNT(*) FROM multipart_uploads
WHERE object_key LIKE @prefix || '%' ESCAPE '\';

-- name: ListMultipartUploadsByPrefix :many
SELECT upload_id, object_key, content_type, created_at
FROM multipart_uploads
WHERE object_key LIKE @prefix || '%' ESCAPE '\'
ORDER BY object_key, created_at
LIMIT @max_uploads;

-- name: ListLegacyMultipartUploads :many
SELECT upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, created_at
FROM multipart_uploads
WHERE encryption_key IS NULL
ORDER BY created_at
LIMIT $1;

-- name: UpdateUploadEncryption :exec
UPDATE multipart_uploads
SET encryption_key = $2, key_id = $3
WHERE upload_id = $1;

-- name: UpdatePartEncryption :exec
UPDATE multipart_parts
SET size_bytes = $3, encrypted = $4, encryption_key = $5, key_id = $6, plaintext_size = $7
WHERE upload_id = $1 AND part_number = $2;
