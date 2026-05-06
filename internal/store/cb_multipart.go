// -------------------------------------------------------------------------------
// CB Decorator  -  MultipartStore
//
// Author: Alex Freidah
//
// Wraps a core.MultipartStore (multipart_uploads + multipart_parts
// state for in-progress uploads) so every call routes through the
// database CircuitBreaker. Keeps the multipart APIs from hanging on an
// unreachable database by returning ErrDBUnavailable instantly when
// the breaker is open.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cbMultipartStore wraps a MultipartStore with circuit-breaker protection.
type cbMultipartStore struct {
	inner core.MultipartStore
	cb    *breaker.CircuitBreaker
}

// NewCBMultipartStore returns a CB-protected view typed as MultipartStore.
func NewCBMultipartStore(inner core.MultipartStore, cb *breaker.CircuitBreaker) core.MultipartStore {
	return &cbMultipartStore{inner: inner, cb: cb}
}

// CreateMultipartUpload forwards to the inner store under the breaker.
func (c *cbMultipartStore) CreateMultipartUpload(ctx context.Context, params *core.CreateMultipartUploadParams) error {
	return breaker.CBCallNoResult(c.cb, func() error {
		return c.inner.CreateMultipartUpload(ctx, params)
	})
}

// GetMultipartUpload forwards to the inner store under the breaker.
func (c *cbMultipartStore) GetMultipartUpload(ctx context.Context, uploadID string) (*core.MultipartUpload, error) {
	return breaker.CBCall(c.cb, func() (*core.MultipartUpload, error) { return c.inner.GetMultipartUpload(ctx, uploadID) })
}

// RecordPart forwards to the inner store under the breaker.
func (c *cbMultipartStore) RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, enc *core.EncryptionMeta) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.RecordPart(ctx, uploadID, partNumber, etag, size, enc) })
}

// GetParts forwards to the inner store under the breaker.
func (c *cbMultipartStore) GetParts(ctx context.Context, uploadID string) ([]core.MultipartPart, error) {
	return breaker.CBCall(c.cb, func() ([]core.MultipartPart, error) { return c.inner.GetParts(ctx, uploadID) })
}

// DeleteMultipartUpload forwards to the inner store under the breaker.
func (c *cbMultipartStore) DeleteMultipartUpload(ctx context.Context, uploadID string) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.DeleteMultipartUpload(ctx, uploadID) })
}

// ListMultipartUploads forwards to the inner store under the breaker.
func (c *cbMultipartStore) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]core.MultipartUpload, error) {
	return breaker.CBCall(c.cb, func() ([]core.MultipartUpload, error) { return c.inner.ListMultipartUploads(ctx, prefix, maxUploads) })
}

// CountActiveMultipartUploads forwards to the inner store under the breaker.
func (c *cbMultipartStore) CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error) {
	return breaker.CBCall(c.cb, func() (int64, error) { return c.inner.CountActiveMultipartUploads(ctx, bucketPrefix) })
}

// GetStaleMultipartUploads forwards to the inner store under the breaker.
func (c *cbMultipartStore) GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]core.MultipartUpload, error) {
	return breaker.CBCall(c.cb, func() ([]core.MultipartUpload, error) { return c.inner.GetStaleMultipartUploads(ctx, olderThan) })
}

// GetMultipartUploadsByBackend forwards to the inner store under the breaker.
func (c *cbMultipartStore) GetMultipartUploadsByBackend(ctx context.Context, backendName string) ([]core.MultipartUpload, error) {
	return breaker.CBCall(c.cb, func() ([]core.MultipartUpload, error) { return c.inner.GetMultipartUploadsByBackend(ctx, backendName) })
}

// ListLegacyMultipartUploads forwards to the inner store under the breaker.
func (c *cbMultipartStore) ListLegacyMultipartUploads(ctx context.Context, limit int) ([]core.MultipartUpload, error) {
	return breaker.CBCall(c.cb, func() ([]core.MultipartUpload, error) { return c.inner.ListLegacyMultipartUploads(ctx, limit) })
}

// UpdateUploadEncryption forwards to the inner store under the breaker.
func (c *cbMultipartStore) UpdateUploadEncryption(ctx context.Context, uploadID string, encryptionKey []byte, keyID string) error {
	return breaker.CBCallNoResult(c.cb, func() error {
		return c.inner.UpdateUploadEncryption(ctx, uploadID, encryptionKey, keyID)
	})
}

// UpdatePartEncryption forwards to the inner store under the breaker.
func (c *cbMultipartStore) UpdatePartEncryption(ctx context.Context, uploadID string, partNumber int, sizeBytes int64, enc *core.EncryptionMeta) error {
	return breaker.CBCallNoResult(c.cb, func() error {
		return c.inner.UpdatePartEncryption(ctx, uploadID, partNumber, sizeBytes, enc)
	})
}
