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

-- name: CreateMultipartUpload :execrows
-- Records the upload only if its backend accepts writes, read from the same view
-- every other admission test uses. Nothing is claimed against the backend's room
-- here: each part is counted by its own row as it lands. Zero rows affected
-- means the backend is being drained and the caller should try the next one.
--
-- Every parameter is cast because it appears in a SELECT list, where a bare
-- parameter takes no type from the INSERT target.
INSERT INTO multipart_uploads (upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, tagging, created_at)
SELECT @upload_id::text, @object_key::text, @backend_name::text,
       sqlc.narg('content_type')::text, sqlc.narg('metadata')::jsonb,
       sqlc.narg('encryption_key')::bytea, sqlc.narg('key_id')::text,
       sqlc.narg('tagging')::text, NOW()
FROM backend_capacity c
WHERE c.backend_name = @backend_name::text AND c.accepting_writes;

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

-- name: ListParts :many
-- One page of an upload's parts: those numbered above after_part, in order.
-- Selects the same columns as GetParts so both map through one converter.
SELECT part_number, etag, plaintext_etag, size_bytes, encrypted, encryption_key, key_id, plaintext_size, created_at
FROM multipart_parts
WHERE upload_id = @upload_id
  AND part_number > @after_part
ORDER BY part_number
LIMIT @row_limit;

-- name: DeleteMultipartUpload :exec
DELETE FROM multipart_uploads
WHERE upload_id = $1;

-- name: ScanMultipartUploads :many
-- Full rows for the abort scans, paged by upload_id. An empty backend filter
-- and a NULL cutoff each match every upload.
SELECT upload_id, object_key, backend_name, content_type, metadata, encryption_key, key_id, created_at
FROM multipart_uploads
WHERE (sqlc.arg(backend_filter)::text = '' OR backend_name = sqlc.arg(backend_filter)::text)
  AND (sqlc.narg(created_before)::timestamptz IS NULL OR created_at < sqlc.narg(created_before)::timestamptz)
  AND upload_id > sqlc.arg(after_upload_id)::text
ORDER BY upload_id
LIMIT sqlc.arg(row_limit);

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
