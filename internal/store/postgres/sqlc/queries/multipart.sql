-- -----------------------------------------------------------------------------
-- Multipart Upload Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for multipart_uploads and multipart_parts. Covers
-- the upload lifecycle (create, lookup, delete), per-part record/list, the
-- prefix-scoped listing the S3 ListMultipartUploads handler needs, and the
-- stale-upload sweep used by the multipart cleanup background worker.
-- -----------------------------------------------------------------------------

-- name: CreateMultipartUpload :exec
INSERT INTO multipart_uploads (upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, tagging, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW());

-- name: GetMultipartUpload :one
-- tagging rides along because CompleteMultipartUpload applies the set the
-- create call carried; the other reads have no use for it and omit it.
SELECT upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, tagging, created_at
FROM multipart_uploads
WHERE upload_id = $1;

-- name: UpsertPart :exec
INSERT INTO multipart_parts (upload_id, part_number, etag, plaintext_etag, size_bytes, encrypted, encryption_key, key_id, plaintext_size, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
ON CONFLICT (upload_id, part_number) DO UPDATE SET
    etag = $3, plaintext_etag = $4, size_bytes = $5, encrypted = $6, encryption_key = $7, key_id = $8, plaintext_size = $9, created_at = NOW();

-- name: GetParts :many
SELECT part_number, etag, plaintext_etag, size_bytes, encrypted, encryption_key, key_id, plaintext_size, created_at
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
