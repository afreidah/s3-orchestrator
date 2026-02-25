// -------------------------------------------------------------------------------
// HTTP Server - S3 Request Routing
//
// Author: Alex Freidah
//
// HTTP server and request router for S3-compatible operations. Implements a
// subset of the S3 API sufficient for basic object storage: PUT, GET, HEAD,
// DELETE. Routes requests to the appropriate handler based on method and query
// parameters. Supports multi-bucket isolation via credential-based bucket
// resolution and internal key prefixing.
// -------------------------------------------------------------------------------

package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/auth"
	"github.com/afreidah/s3-orchestrator/internal/storage"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// -------------------------------------------------------------------------
// SERVER
// -------------------------------------------------------------------------

// Server handles HTTP requests and routes them to the backend manager.
type Server struct {
	Manager       *storage.BackendManager
	bucketAuth    atomic.Pointer[auth.BucketRegistry]
	MaxObjectSize int64 // Max upload body size in bytes
}

// SetBucketAuth atomically replaces the bucket authentication registry.
// Safe to call concurrently with request handling.
func (s *Server) SetBucketAuth(br *auth.BucketRegistry) {
	s.bucketAuth.Store(br)
}

// GetBucketAuth returns the current bucket authentication registry.
func (s *Server) GetBucketAuth() *auth.BucketRegistry {
	return s.bucketAuth.Load()
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	method := r.Method

	// --- Generate or adopt request ID ---
	requestID := r.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = audit.NewID()
	}
	ctx := audit.WithRequestID(r.Context(), requestID)
	w.Header().Set("X-Amz-Request-Id", requestID)

	// --- Track inflight requests ---
	telemetry.InflightRequests.WithLabelValues(method).Inc()
	defer telemetry.InflightRequests.WithLabelValues(method).Dec()

	// --- Auth: resolve which bucket these credentials authorize ---
	authorizedBucket, err := s.GetBucketAuth().AuthenticateAndResolveBucket(r)
	if err != nil {
		s.recordRequest(method, http.StatusForbidden, start, 0, 0)
		audit.Log(ctx, "s3.AuthFailure",
			slog.String("method", method),
			slog.String("path", r.URL.Path),
			slog.String("remote", r.RemoteAddr),
			slog.String("error", err.Error()),
			slog.Int("status", http.StatusForbidden),
			slog.Duration("duration", time.Since(start)),
		)
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access denied")
		return
	}

	// --- Parse path ---
	bucket, key, ok := parsePath(r.URL.Path)
	if !ok {
		s.recordRequest(method, http.StatusBadRequest, start, 0, 0)
		audit.Log(ctx, "s3.InvalidPath",
			slog.String("method", method),
			slog.String("path", r.URL.Path),
			slog.String("remote", r.RemoteAddr),
			slog.Int("status", http.StatusBadRequest),
			slog.Duration("duration", time.Since(start)),
		)
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "Invalid path format")
		return
	}

	// --- Verify path bucket matches authorized bucket ---
	if bucket != authorizedBucket {
		s.recordRequest(method, http.StatusForbidden, start, 0, 0)
		audit.Log(ctx, "s3.BucketMismatch",
			slog.String("method", method),
			slog.String("path", r.URL.Path),
			slog.String("remote", r.RemoteAddr),
			slog.String("authorized_bucket", authorizedBucket),
			slog.String("requested_bucket", bucket),
			slog.Int("status", http.StatusForbidden),
			slog.Duration("duration", time.Since(start)),
		)
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access denied")
		return
	}

	// --- Prefix key for internal storage isolation ---
	internalKey := bucket + "/" + key

	// --- Start tracing span ---
	ctx, span := telemetry.StartSpan(ctx, fmt.Sprintf("HTTP %s", method),
		append(telemetry.RequestAttributes(method, r.URL.Path, bucket, key, r.RemoteAddr),
			telemetry.AttrRequestID.String(requestID))...,
	)
	defer span.End()

	// --- Route by method and track operation name ---
	var status int
	var requestSize, responseSize int64
	var operation string

	// --- Bucket-level operations (no key) ---
	if key == "" {
		if method != http.MethodGet {
			s.recordRequest(method, http.StatusMethodNotAllowed, start, 0, 0)
			audit.Log(ctx, "s3.MethodNotAllowed",
				slog.String("method", method),
				slog.String("path", r.URL.Path),
				slog.String("remote", r.RemoteAddr),
				slog.String("bucket", bucket),
				slog.Int("status", http.StatusMethodNotAllowed),
				slog.Duration("duration", time.Since(start)),
			)
			writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not supported for bucket")
			span.SetStatus(codes.Error, "method not allowed for bucket")
			return
		}
		operation = "ListObjectsV2"
		status, err = s.handleListObjectsV2(ctx, w, r, bucket)
	} else {
		// --- Multipart upload routing ---
		query := r.URL.Query()
		_, hasUploads := query["uploads"]
		uploadID := query.Get("uploadId")

		switch {
		case hasUploads && method == http.MethodPost:
			operation = "CreateMultipartUpload"
			status, err = s.handleCreateMultipartUpload(ctx, w, r, bucket, key, internalKey)
		case uploadID != "":
			switch method {
			case http.MethodPut:
				operation = "UploadPart"
				requestSize = r.ContentLength
				status, err = s.handleUploadPart(ctx, w, r, internalKey)
			case http.MethodPost:
				operation = "CompleteMultipartUpload"
				status, err = s.handleCompleteMultipartUpload(ctx, w, r, bucket, key)
			case http.MethodDelete:
				operation = "AbortMultipartUpload"
				status, err = s.handleAbortMultipartUpload(ctx, w, uploadID)
			case http.MethodGet:
				operation = "ListParts"
				status, err = s.handleListParts(ctx, w, r, bucket, key, internalKey)
			default:
				s.recordRequest(method, http.StatusMethodNotAllowed, start, 0, 0)
				audit.Log(ctx, "s3.MethodNotAllowed",
					slog.String("method", method),
					slog.String("path", r.URL.Path),
					slog.String("remote", r.RemoteAddr),
					slog.String("bucket", bucket),
					slog.Int("status", http.StatusMethodNotAllowed),
					slog.Duration("duration", time.Since(start)),
				)
				writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not supported")
				span.SetStatus(codes.Error, "method not allowed")
				return
			}
		default:
			switch method {
			case http.MethodPut:
				if copySource := r.Header.Get("X-Amz-Copy-Source"); copySource != "" {
					operation = "CopyObject"
					status, err = s.handleCopyObject(ctx, w, r, bucket, internalKey, copySource)
				} else {
					operation = "PutObject"
					requestSize = r.ContentLength
					status, err = s.handlePut(ctx, w, r, internalKey)
				}
			case http.MethodGet:
				operation = "GetObject"
				status, responseSize, err = s.handleGet(ctx, w, r, internalKey)
			case http.MethodHead:
				operation = "HeadObject"
				status, err = s.handleHead(ctx, w, r, internalKey)
			case http.MethodDelete:
				operation = "DeleteObject"
				status, err = s.handleDelete(ctx, w, r, internalKey)
			default:
				s.recordRequest(method, http.StatusMethodNotAllowed, start, 0, 0)
				audit.Log(ctx, "s3.MethodNotAllowed",
					slog.String("method", method),
					slog.String("path", r.URL.Path),
					slog.String("remote", r.RemoteAddr),
					slog.String("bucket", bucket),
					slog.Int("status", http.StatusMethodNotAllowed),
					slog.Duration("duration", time.Since(start)),
				)
				writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not supported")
				span.SetStatus(codes.Error, "method not allowed")
				return
			}
		}
	}

	// --- Record metrics ---
	s.recordRequest(method, status, start, requestSize, responseSize)

	// --- Update span status ---
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	}
	span.SetAttributes(attribute.Int("http.status_code", status))

	// --- Audit log ---
	elapsed := time.Since(start)
	auditAttrs := []slog.Attr{
		slog.String("operation", operation),
		slog.String("method", method),
		slog.String("path", r.URL.Path),
		slog.String("remote", r.RemoteAddr),
		slog.String("bucket", bucket),
		slog.Int("status", status),
		slog.Duration("duration", elapsed),
	}
	if key != "" {
		auditAttrs = append(auditAttrs, slog.String("key", key))
	}
	if requestSize > 0 {
		auditAttrs = append(auditAttrs, slog.Int64("request_size", requestSize))
	}
	if responseSize > 0 {
		auditAttrs = append(auditAttrs, slog.Int64("response_size", responseSize))
	}
	if err != nil {
		auditAttrs = append(auditAttrs, slog.String("error", err.Error()))
	}
	audit.Log(ctx, "s3."+operation, auditAttrs...)
}

// recordRequest updates Prometheus metrics for a completed request.
func (s *Server) recordRequest(method string, status int, start time.Time, reqSize, respSize int64) {
	statusStr := strconv.Itoa(status)
	telemetry.RequestsTotal.WithLabelValues(method, statusStr).Inc()
	telemetry.RequestDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())

	if reqSize > 0 {
		telemetry.RequestSize.WithLabelValues(method).Observe(float64(reqSize))
	}
	if respSize > 0 {
		telemetry.ResponseSize.WithLabelValues(method).Observe(float64(respSize))
	}
}
