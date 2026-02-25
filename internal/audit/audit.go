// -------------------------------------------------------------------------------
// Audit - Request ID Tracing and Structured Audit Logging
//
// Author: Alex Freidah
//
// Context-based request ID propagation and structured audit logging. Generates
// unique request IDs for S3 API requests (honoring client-provided X-Request-Id)
// and correlation IDs for internal operations. Emits structured slog entries
// with an "audit" marker for log pipeline filtering.
// -------------------------------------------------------------------------------

package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// -------------------------------------------------------------------------
// CONTEXT KEYS
// -------------------------------------------------------------------------

type contextKey int

const (
	requestIDKey contextKey = iota
)

// -------------------------------------------------------------------------
// REQUEST ID
// -------------------------------------------------------------------------

// NewID generates a hex-encoded 16-byte random ID suitable for request
// correlation. Falls back to a timestamp-based ID if crypto/rand fails.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: should never happen with a healthy OS
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(b)
}

// WithRequestID stores a request ID in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID extracts the request ID from the context. Returns empty string
// if no request ID is set.
func RequestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// -------------------------------------------------------------------------
// AUDIT LOGGING
// -------------------------------------------------------------------------

// Log emits a structured audit log entry at Info level. Automatically
// includes the request ID from context and increments the audit event
// counter.
func Log(ctx context.Context, event string, attrs ...slog.Attr) {
	telemetry.AuditEventsTotal.WithLabelValues(event).Inc()

	base := []slog.Attr{
		slog.Bool("audit", true),
		slog.String("event", event),
	}

	if id := RequestID(ctx); id != "" {
		base = append(base, slog.String("request_id", id))
	}

	base = append(base, attrs...)

	args := make([]any, len(base))
	for i, a := range base {
		args[i] = a
	}

	slog.LogAttrs(ctx, slog.LevelInfo, "audit", base...)
}
