// -------------------------------------------------------------------------------
// Backend - S3-Compatible Storage Client
//
// Author: Alex Freidah
//
// Storage backend implementation using AWS SDK v2. Connects to any S3-compatible
// endpoint (OCI, AWS, B2, MinIO) via custom endpoint configuration. The same code
// works for all providers since they all speak the S3 protocol.
// -------------------------------------------------------------------------------

package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithymiddleware "github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
)

// -------------------------------------------------------------------------
// INTERFACE
// -------------------------------------------------------------------------

// GetObjectResult holds the response from a GetObject call.
type GetObjectResult struct {
	Body         io.ReadCloser
	Size         int64
	ContentType  string
	ETag         string
	ContentRange string
	LastModified time.Time
	Metadata     map[string]string
}

// HeadObjectResult holds the response from a HeadObject call.
type HeadObjectResult struct {
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
	Metadata     map[string]string
}

// ObjectBackend defines the interface for object storage operations.
type ObjectBackend interface {
	PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (etag string, err error)
	GetObject(ctx context.Context, key string, rangeHeader string) (*GetObjectResult, error)
	HeadObject(ctx context.Context, key string) (*HeadObjectResult, error)
	DeleteObject(ctx context.Context, key string) error
}

// -------------------------------------------------------------------------
// S3 BACKEND IMPLEMENTATION
// -------------------------------------------------------------------------

// S3Backend implements ObjectBackend using AWS SDK v2.
type S3Backend struct {
	client          *s3.Client
	bucket          string
	name            string
	endpoint        string
	unsignedPayload bool
}

// NewS3Backend creates a new S3-compatible backend client. Uses BaseEndpoint
// to direct requests to the configured provider instead of AWS.
func NewS3Backend(cfg *config.BackendConfig) (*S3Backend, error) {
	// --- Create S3 client with custom endpoint ---
	opts := s3.Options{
		Region:       cfg.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: cfg.ForcePathStyle,
	}
	if cfg.DisableChecksum {
		opts.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		opts.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	}
	if cfg.StripSDKHeaders {
		opts.APIOptions = append(opts.APIOptions, stripSDKHeadersMiddleware)
	}
	client := s3.New(opts)

	// Default to unsigned payload (streaming) to avoid buffering entire
	// objects in memory for SigV4 payload hashing. When the user has not
	// set the option explicitly, auto-disable over plain HTTP since AWS S3
	// rejects the UNSIGNED-PAYLOAD sentinel without TLS. An explicit
	// unsigned_payload: true in config is always respected (MinIO and most
	// S3-compatible backends accept it over HTTP).
	unsignedPayload := true
	if cfg.UnsignedPayload != nil {
		unsignedPayload = *cfg.UnsignedPayload
	} else if !strings.HasPrefix(cfg.Endpoint, "https") {
		unsignedPayload = false
	}

	return &S3Backend{
		client:          client,
		bucket:          cfg.Bucket,
		name:            cfg.Name,
		endpoint:        cfg.Endpoint,
		unsignedPayload: unsignedPayload,
	}, nil
}

// -------------------------------------------------------------------------
// CRUD OPERATIONS
// -------------------------------------------------------------------------

// PutObject uploads an object to the backend.
func (b *S3Backend) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	const operation = "PutObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := telemetry.StartSpan(ctx, "Backend "+operation,
		telemetry.BackendAttributes(operation, b.name, b.endpoint, b.bucket, key)...,
	)
	defer span.End()

	// When unsigned_payload is enabled (default), the SDK skips computing
	// a SHA-256 payload hash and accepts a non-seekable io.Reader directly,
	// avoiding buffering the entire body in memory. Body integrity is
	// protected by TLS at the transport layer. When disabled, the body is
	// buffered into memory so the SDK can compute the SigV4 payload hash.
	putBody := body
	var opts []func(*s3.Options)
	if b.unsignedPayload {
		opts = append(opts, withUnsignedPayload)
	} else {
		if _, ok := body.(io.ReadSeeker); !ok {
			data, err := io.ReadAll(body)
			if err != nil {
				span.SetStatus(codes.Error, err.Error())
				return "", fmt.Errorf("failed to read body: %w", err)
			}
			putBody = bytes.NewReader(data)
		}
	}

	input := &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(key),
		Body:          putBody,
		ContentLength: aws.Int64(size),
	}

	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}
	if len(metadata) > 0 {
		input.Metadata = metadata
	}

	result, err := b.client.PutObject(ctx, input, opts...)

	// --- Record metrics ---
	b.recordOperation(operation, start, err)

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", fmt.Errorf("put object failed: %w", err)
	}

	etag := ""
	if result.ETag != nil {
		etag = *result.ETag
	}
	return etag, nil
}

// GetObject retrieves an object from the backend. When rangeHeader is non-empty
// (e.g. "bytes=0-99"), it is passed through to S3 and the response includes a
// contentRange value (e.g. "bytes 0-99/1000") for 206 Partial Content responses.
func (b *S3Backend) GetObject(ctx context.Context, key string, rangeHeader string) (*GetObjectResult, error) {
	const operation = "GetObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := telemetry.StartSpan(ctx, "Backend "+operation,
		telemetry.BackendAttributes(operation, b.name, b.endpoint, b.bucket, key)...,
	)
	defer span.End()

	input := &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}
	if rangeHeader != "" {
		input.Range = aws.String(rangeHeader)
	}

	result, err := b.client.GetObject(ctx, input)

	// --- Record metrics ---
	b.recordOperation(operation, start, err)

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, fmt.Errorf("get object failed: %w", err)
	}

	out := &GetObjectResult{Body: result.Body}

	if result.ContentLength != nil {
		out.Size = *result.ContentLength
	}
	if result.ContentType != nil {
		out.ContentType = *result.ContentType
	} else {
		out.ContentType = "application/octet-stream"
	}
	if result.ETag != nil {
		out.ETag = *result.ETag
	}
	if result.ContentRange != nil {
		out.ContentRange = *result.ContentRange
	}
	if result.LastModified != nil {
		out.LastModified = *result.LastModified
	}
	if len(result.Metadata) > 0 {
		out.Metadata = result.Metadata
	}

	return out, nil
}

// HeadObject retrieves object metadata without the body.
func (b *S3Backend) HeadObject(ctx context.Context, key string) (*HeadObjectResult, error) {
	const operation = "HeadObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := telemetry.StartSpan(ctx, "Backend "+operation,
		telemetry.BackendAttributes(operation, b.name, b.endpoint, b.bucket, key)...,
	)
	defer span.End()

	result, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})

	// --- Record metrics ---
	b.recordOperation(operation, start, err)

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, fmt.Errorf("head object failed: %w", err)
	}

	out := &HeadObjectResult{
		ContentType: "application/octet-stream",
	}
	if result.ContentLength != nil {
		out.Size = *result.ContentLength
	}
	if result.ContentType != nil {
		out.ContentType = *result.ContentType
	}
	if result.ETag != nil {
		out.ETag = *result.ETag
	}
	if result.LastModified != nil {
		out.LastModified = *result.LastModified
	}
	if len(result.Metadata) > 0 {
		out.Metadata = result.Metadata
	}

	return out, nil
}

// DeleteObject removes an object from the backend.
func (b *S3Backend) DeleteObject(ctx context.Context, key string) error {
	const operation = "DeleteObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := telemetry.StartSpan(ctx, "Backend "+operation,
		telemetry.BackendAttributes(operation, b.name, b.endpoint, b.bucket, key)...,
	)
	defer span.End()

	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})

	// --- Record metrics ---
	b.recordOperation(operation, start, err)

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return fmt.Errorf("delete object failed: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// LISTING
// -------------------------------------------------------------------------

// ListedObject holds metadata for a single object returned by S3 ListObjects.
type ListedObject struct {
	Key       string
	SizeBytes int64
}

// ListObjects iterates all objects in the backend bucket with the given prefix,
// calling fn for each page of results. Uses ListObjectsV2 pagination internally.
func (b *S3Backend) ListObjects(ctx context.Context, prefix string, fn func([]ListedObject) error) error {
	const operation = "ListObjectsV2"

	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
	}
	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}

	paginator := s3.NewListObjectsV2Paginator(b.client, input)
	for paginator.HasMorePages() {
		start := time.Now()
		page, err := paginator.NextPage(ctx)
		b.recordOperation(operation, start, err)

		if err != nil {
			return fmt.Errorf("list objects failed: %w", err)
		}

		objects := make([]ListedObject, len(page.Contents))
		for i, obj := range page.Contents {
			key := ""
			if obj.Key != nil {
				key = *obj.Key
			}
			size := int64(0)
			if obj.Size != nil {
				size = *obj.Size
			}
			objects[i] = ListedObject{Key: key, SizeBytes: size}
		}

		if len(objects) > 0 {
			if err := fn(objects); err != nil {
				return err
			}
		}
	}

	return nil
}

// -------------------------------------------------------------------------
// METRICS HELPER
// -------------------------------------------------------------------------

// stripSDKHeadersMiddleware removes AWS SDK v2-specific headers and query
// parameters before request signing. GCS (and potentially other S3-compatible
// backends) fail signature verification when these non-standard headers are
// included in the signed header set.
func stripSDKHeadersMiddleware(stack *smithymiddleware.Stack) error {
	return stack.Finalize.Insert(smithymiddleware.FinalizeMiddlewareFunc(
		"StripSDKHeaders",
		func(ctx context.Context, in smithymiddleware.FinalizeInput, next smithymiddleware.FinalizeHandler) (smithymiddleware.FinalizeOutput, smithymiddleware.Metadata, error) {
			req, ok := in.Request.(*smithyhttp.Request)
			if ok {
				q := req.URL.Query()
				q.Del("x-id")
				req.URL.RawQuery = q.Encode()

				req.Header.Del("Amz-Sdk-Invocation-Id")
				req.Header.Del("Amz-Sdk-Request")
				req.Header.Del("Accept-Encoding")
			}
			return next.HandleFinalize(ctx, in)
		},
	), "Signing", smithymiddleware.Before)
}

// withUnsignedPayload is an S3 per-request option that replaces the payload
// SHA-256 computation with the UNSIGNED-PAYLOAD sentinel. This allows the SDK
// to accept a non-seekable io.Reader body without buffering the entire object
// into memory. Body integrity is still protected by TLS at the transport layer.
func withUnsignedPayload(o *s3.Options) {
	o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
}

// recordOperation updates Prometheus metrics for a backend operation.
func (b *S3Backend) recordOperation(operation string, start time.Time, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}

	telemetry.BackendRequestsTotal.WithLabelValues(operation, b.name, status).Inc()
	telemetry.BackendDuration.WithLabelValues(operation, b.name).Observe(time.Since(start).Seconds())
}
