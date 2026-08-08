// -------------------------------------------------------------------------------
// Backend - S3-Compatible Storage Client
//
// Author: Alex Freidah
//
// Storage backend implementation using AWS SDK v2. Connects to any S3-compatible
// endpoint (OCI, AWS, B2, MinIO) via custom endpoint configuration. The same code
// works for all providers since they all speak the S3 protocol.
// -------------------------------------------------------------------------------

package backend

//go:generate mockgen -destination=mock_generated_test.go -package=backend github.com/afreidah/s3-orchestrator/internal/backend ObjectBackend

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithymiddleware "github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/materialize"
)

// -------------------------------------------------------------------------
// INTERFACE
// -------------------------------------------------------------------------

// spanPrefix is prepended to every OpenTelemetry span name produced by
// this package so traces clearly attribute the call to the backend
// layer ("Backend GetObject") versus the manager layer ("Manager
// GetObject") in the same trace.
const spanPrefix = "Backend "

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

// newBackendTransport creates a tuned HTTP transport for a single S3 backend.
// Each backend gets its own transport so connection pools are isolated and
// sized for the proxy's concurrent workload (rebalancer, replicator, parallel
// PUTs/GETs). IdleConnTimeout bounds DNS staleness: idle connections are
// recycled within 90 s, forcing a fresh DNS resolution on the next dial.
func newBackendTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       200,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// NewS3Backend creates a new S3-compatible backend client. Uses BaseEndpoint
// to direct requests to the configured provider instead of AWS. Each backend
// gets a dedicated HTTP transport with tuned connection pool settings.
// ctx is used for the default-chain credential probe (IMDS, SSO, STS); the
// resulting provider continues to refresh in the background after Init.
func NewS3Backend(ctx context.Context, cfg *config.BackendConfig) (*S3Backend, error) {
	creds, err := resolveCredentials(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve credentials for backend %q: %w", cfg.Name, err)
	}
	// --- Create S3 client with custom endpoint and transport ---
	opts := s3.Options{
		Region:       cfg.Region,
		Credentials:  creds,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: cfg.ForcePathStyle,
		HTTPClient:   &http.Client{Transport: newBackendTransport()},
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

// preparePutBody resolves the body and request options for a single PutObject
// call, plus a cleanup the caller must defer (always safe to call). In
// unsigned-payload mode (default) it tags the request so the SDK skips the
// SigV4 payload hash and streams a non-seekable body directly; integrity is
// protected by TLS. In signed-payload mode the SDK needs a seekable body to
// hash, so a non-seekable stream is materialized (memory below
// materialize.MemThreshold, self-unlinking tempfile above) rather than buffered
// whole on the heap, which would OOM the proxy on a large upload.
func (b *S3Backend) preparePutBody(body io.Reader, size int64) (io.Reader, []func(*s3.Options), func(), error) {
	// noop is the cleanup for the paths that materialize nothing (unsigned
	// mode and already-seekable bodies), returned so every caller can defer
	// unconditionally; only the tempfile path has an fd to release.
	noop := func() {
		// Nothing to release on the non-materialized paths.
	}
	if b.unsignedPayload {
		return body, []func(*s3.Options){withUnsignedPayload}, noop, nil
	}
	if _, ok := body.(io.ReadSeeker); ok {
		return body, nil, noop, nil
	}
	mb, err := materialize.New(body, size, nil)
	if err != nil {
		return nil, nil, noop, fmt.Errorf("materialize signed-payload body: %w", err)
	}
	seekable, err := mb.Reader()
	if err != nil {
		mb.Cleanup()
		return nil, nil, noop, fmt.Errorf("read materialized body: %w", err)
	}
	return seekable, nil, mb.Cleanup, nil
}

// PutObject uploads an object to the backend.
func (b *S3Backend) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	const operation = "PutObject"
	return observe.Run(ctx,
		observe.Client(spanPrefix+operation,
			telemetry.BackendAttributes(operation, b.name, b.endpoint, b.bucket, key),
			b.recordOperation),
		func(ctx context.Context) (string, error) {
			putBody, opts, cleanup, err := b.preparePutBody(body, size)
			if err != nil {
				return "", err
			}
			defer cleanup()

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
			if err != nil {
				return "", fmt.Errorf("put object failed: %w", err)
			}
			etag := ""
			if result.ETag != nil {
				etag = *result.ETag
			}
			return etag, nil
		})
}

// GetObject retrieves an object from the backend. When rangeHeader is non-empty
// (e.g. "bytes=0-99"), it is passed through to S3 and the response includes a
// contentRange value (e.g. "bytes 0-99/1000") for 206 Partial Content responses.
func (b *S3Backend) GetObject(ctx context.Context, key string, rangeHeader string) (*GetObjectResult, error) {
	const operation = "GetObject"
	return observe.Run(ctx,
		observe.Client(spanPrefix+operation,
			telemetry.BackendAttributes(operation, b.name, b.endpoint, b.bucket, key),
			b.recordOperation),
		func(ctx context.Context) (*GetObjectResult, error) {
			input := &s3.GetObjectInput{
				Bucket: aws.String(b.bucket),
				Key:    aws.String(key),
			}
			if rangeHeader != "" {
				input.Range = aws.String(rangeHeader)
			}
			result, err := b.client.GetObject(ctx, input)
			if err != nil {
				return nil, fmt.Errorf("get object failed: %w", err)
			}
			return mapGetObjectResult(result), nil
		})
}

// objectAttrs holds the object-metadata fields shared by the GetObject and
// HeadObject SDK responses after nil-pointer unwrapping.
type objectAttrs struct {
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
	Metadata     map[string]string
}

// objectAttrsFromSDK unwraps the nil-able metadata fields common to the
// GetObject and HeadObject SDK outputs, defaulting ContentType to
// application/octet-stream when the backend omits it.
func objectAttrsFromSDK(contentLength *int64, contentType, etag *string, lastModified *time.Time, metadata map[string]string) objectAttrs {
	attrs := objectAttrs{ContentType: "application/octet-stream"}
	if contentLength != nil {
		attrs.Size = *contentLength
	}
	if contentType != nil {
		attrs.ContentType = *contentType
	}
	if etag != nil {
		attrs.ETag = *etag
	}
	if lastModified != nil {
		attrs.LastModified = *lastModified
	}
	if len(metadata) > 0 {
		attrs.Metadata = metadata
	}
	return attrs
}

// mapGetObjectResult normalises an SDK GetObjectOutput into the
// package-local GetObjectResult.
func mapGetObjectResult(result *s3.GetObjectOutput) *GetObjectResult {
	a := objectAttrsFromSDK(result.ContentLength, result.ContentType, result.ETag, result.LastModified, result.Metadata)
	out := &GetObjectResult{
		Body:         result.Body,
		Size:         a.Size,
		ContentType:  a.ContentType,
		ETag:         a.ETag,
		LastModified: a.LastModified,
		Metadata:     a.Metadata,
	}
	if result.ContentRange != nil {
		out.ContentRange = *result.ContentRange
	}
	return out
}

// HeadObject retrieves object metadata without the body.
func (b *S3Backend) HeadObject(ctx context.Context, key string) (*HeadObjectResult, error) {
	const operation = "HeadObject"
	return observe.Run(ctx,
		observe.Client(spanPrefix+operation,
			telemetry.BackendAttributes(operation, b.name, b.endpoint, b.bucket, key),
			b.recordOperation),
		func(ctx context.Context) (*HeadObjectResult, error) {
			result, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
				Bucket: aws.String(b.bucket),
				Key:    aws.String(key),
			})
			if err != nil {
				return nil, fmt.Errorf("head object failed: %w", err)
			}

			a := objectAttrsFromSDK(result.ContentLength, result.ContentType, result.ETag, result.LastModified, result.Metadata)
			return &HeadObjectResult{
				Size:         a.Size,
				ContentType:  a.ContentType,
				ETag:         a.ETag,
				LastModified: a.LastModified,
				Metadata:     a.Metadata,
			}, nil
		})
}

// DeleteObject removes an object from the backend.
func (b *S3Backend) DeleteObject(ctx context.Context, key string) error {
	const operation = "DeleteObject"
	return observe.RunErr(ctx,
		observe.Client(spanPrefix+operation,
			telemetry.BackendAttributes(operation, b.name, b.endpoint, b.bucket, key),
			b.recordOperation),
		func(ctx context.Context) error {
			_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(b.bucket),
				Key:    aws.String(key),
			})
			if err != nil {
				return fmt.Errorf("delete object failed: %w", err)
			}
			return nil
		})
}

// CopyObject performs a server-side copy from srcKey to dstKey within
// the same backend bucket. Used by the proxy's same-backend copy fast
// path to avoid materializing the object through the orchestrator.
//
// When contentType or metadata is provided the SDK sends
// MetadataDirective=REPLACE so the destination uses the supplied
// values; otherwise the directive defaults to COPY and S3 preserves
// the source's content type and user metadata. The CopySource string
// is URL-encoded because S3 requires escaping for keys containing
// slashes or other reserved characters.
func (b *S3Backend) CopyObject(ctx context.Context, srcKey, dstKey, contentType string, metadata map[string]string) (string, error) {
	const operation = "CopyObject"
	return observe.Run(ctx,
		observe.Client(spanPrefix+operation,
			telemetry.BackendAttributes(operation, b.name, b.endpoint, b.bucket, dstKey),
			b.recordOperation),
		func(ctx context.Context) (string, error) {
			input := &s3.CopyObjectInput{
				Bucket:     aws.String(b.bucket),
				Key:        aws.String(dstKey),
				CopySource: aws.String(url.PathEscape(b.bucket + "/" + srcKey)),
			}
			if contentType != "" {
				input.ContentType = aws.String(contentType)
				input.MetadataDirective = "REPLACE"
			}
			if len(metadata) > 0 {
				input.Metadata = metadata
				input.MetadataDirective = "REPLACE"
			}
			result, err := b.client.CopyObject(ctx, input)
			if err != nil {
				return "", fmt.Errorf("copy object failed: %w", err)
			}
			etag := ""
			if result.CopyObjectResult != nil && result.CopyObjectResult.ETag != nil {
				etag = *result.CopyObjectResult.ETag
			}
			return etag, nil
		})
}

// -------------------------------------------------------------------------
// LISTING
// -------------------------------------------------------------------------

// ListedObject holds metadata for a single object returned by S3 ListObjects.
type ListedObject struct {
	Key       string
	SizeBytes int64
}

// ListObjects iterates all objects in the backend bucket with the given
// prefix, calling fn for each page of results. Uses ListObjectsV2
// pagination internally; per-page metric emission happens inside
// nextListPage, so the outer span has no recorder.
func (b *S3Backend) ListObjects(ctx context.Context, prefix string, fn func([]ListedObject) error) error {
	const operation = "ListObjectsV2"
	return observe.RunErr(ctx,
		observe.Client(spanPrefix+operation,
			telemetry.BackendAttributes(operation, b.name, b.endpoint, b.bucket, prefix),
			nil),
		func(ctx context.Context) error {
			paginator := s3.NewListObjectsV2Paginator(b.client, listObjectsInput(b.bucket, prefix))
			for paginator.HasMorePages() {
				page, err := b.nextListPage(ctx, paginator, operation)
				if err != nil {
					return err
				}
				objects := convertListPage(page)
				if len(objects) == 0 {
					continue
				}
				if err := fn(objects); err != nil {
					return err
				}
			}
			return nil
		})
}

// listObjectsInput builds the ListObjectsV2 input for the given bucket and
// optional prefix.
func listObjectsInput(bucket, prefix string) *s3.ListObjectsV2Input {
	input := &s3.ListObjectsV2Input{Bucket: aws.String(bucket)}
	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}
	return input
}

// nextListPage fetches the next paginator page and records its operation
// metrics. Wraps the SDK error with context.
func (b *S3Backend) nextListPage(ctx context.Context, paginator *s3.ListObjectsV2Paginator, operation string) (*s3.ListObjectsV2Output, error) {
	start := time.Now()
	page, err := paginator.NextPage(ctx)
	b.recordOperation(operation, start, err)
	if err != nil {
		return nil, fmt.Errorf("list objects failed: %w", err)
	}
	return page, nil
}

// convertListPage maps SDK pointer-nullable Content objects to the flat
// ListedObject shape callers expect.
func convertListPage(page *s3.ListObjectsV2Output) []ListedObject {
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
	return objects
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

// resolveCredentials selects the credentials provider for cfg based on
// CredentialSource. "static" uses the configured access/secret keys;
// "default_chain" delegates to the AWS SDK default chain (env, EC2 IMDS,
// SSO, ~/.aws, STS). The config validator has already rejected an
// unknown source by the time this runs, so the default arm is reached
// only via direct test wiring.
func resolveCredentials(ctx context.Context, cfg *config.BackendConfig) (aws.CredentialsProvider, error) {
	switch cfg.CredentialSource {
	case "", config.CredentialSourceStatic:
		return credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""), nil
	case config.CredentialSourceDefaultChain:
		loaded, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
		if err != nil {
			return nil, fmt.Errorf("load default credential chain: %w", err)
		}
		return loaded.Credentials, nil
	default:
		return nil, fmt.Errorf("unsupported credential_source %q", cfg.CredentialSource)
	}
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
