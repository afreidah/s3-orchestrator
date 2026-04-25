// -------------------------------------------------------------------------------
// CB Decorator — MultipartStore
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// cbMultipartStore wraps a MultipartStore with circuit-breaker protection.
type cbMultipartStore struct {
	inner MultipartStore
	cb    *breaker.CircuitBreaker
}

// NewCBMultipartStore returns a CB-protected view typed as MultipartStore.
func NewCBMultipartStore(inner MultipartStore, cb *breaker.CircuitBreaker) MultipartStore {
	return &cbMultipartStore{inner: inner, cb: cb}
}

// CreateMultipartUpload forwards to the inner store under the breaker.
func (c *cbMultipartStore) CreateMultipartUpload(ctx context.Context, uploadID, key, backend, contentType string, metadata map[string]string) error {
	return breaker.CBCallNoResult(c.cb, func() error {
		return c.inner.CreateMultipartUpload(ctx, uploadID, key, backend, contentType, metadata)
	})
}

// GetMultipartUpload forwards to the inner store under the breaker.
func (c *cbMultipartStore) GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error) {
	return breaker.CBCall(c.cb, func() (*MultipartUpload, error) { return c.inner.GetMultipartUpload(ctx, uploadID) })
}

// RecordPart forwards to the inner store under the breaker.
func (c *cbMultipartStore) RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, enc *EncryptionMeta) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.RecordPart(ctx, uploadID, partNumber, etag, size, enc) })
}

// GetParts forwards to the inner store under the breaker.
func (c *cbMultipartStore) GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error) {
	return breaker.CBCall(c.cb, func() ([]MultipartPart, error) { return c.inner.GetParts(ctx, uploadID) })
}

// DeleteMultipartUpload forwards to the inner store under the breaker.
func (c *cbMultipartStore) DeleteMultipartUpload(ctx context.Context, uploadID string) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.DeleteMultipartUpload(ctx, uploadID) })
}

// ListMultipartUploads forwards to the inner store under the breaker.
func (c *cbMultipartStore) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]MultipartUpload, error) {
	return breaker.CBCall(c.cb, func() ([]MultipartUpload, error) { return c.inner.ListMultipartUploads(ctx, prefix, maxUploads) })
}

// CountActiveMultipartUploads forwards to the inner store under the breaker.
func (c *cbMultipartStore) CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error) {
	return breaker.CBCall(c.cb, func() (int64, error) { return c.inner.CountActiveMultipartUploads(ctx, bucketPrefix) })
}

// GetStaleMultipartUploads forwards to the inner store under the breaker.
func (c *cbMultipartStore) GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]MultipartUpload, error) {
	return breaker.CBCall(c.cb, func() ([]MultipartUpload, error) { return c.inner.GetStaleMultipartUploads(ctx, olderThan) })
}

// GetMultipartUploadsByBackend forwards to the inner store under the breaker.
func (c *cbMultipartStore) GetMultipartUploadsByBackend(ctx context.Context, backendName string) ([]MultipartUpload, error) {
	return breaker.CBCall(c.cb, func() ([]MultipartUpload, error) { return c.inner.GetMultipartUploadsByBackend(ctx, backendName) })
}