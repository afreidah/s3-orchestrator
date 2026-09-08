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
	"sync/atomic"
)

// onEvent holds an optional callback invoked for each audit event with the
// event name. Set via SetOnEvent at startup to integrate audit logging with
// metrics (e.g. Prometheus counters). When nil, audit logging still works  -
// events are emitted via slog but no counter is incremented.
var onEvent atomic.Pointer[func(event string)]

// SetOnEvent registers a callback invoked for each audit event. Pass nil to
// clear a previously registered callback.
func SetOnEvent(fn func(event string)) {
	if fn == nil {
		onEvent.Store(nil)
	} else {
		onEvent.Store(&fn)
	}
}

// -------------------------------------------------------------------------
// CONTEXT KEYS
// -------------------------------------------------------------------------

// contextKey is an unexported integer type used to namespace audit-
// package values stored in context.Context. Using an unexported type
// guarantees no other package can collide with these keys.
type contextKey int

// requestIDKey and userKey are the context keys the correlation ID and the
// authenticated identity are stored under.
const (
	requestIDKey contextKey = iota
	userKey
)

// -------------------------------------------------------------------------
// REQUEST ID
// -------------------------------------------------------------------------

// NewID generates a hex-encoded 16-byte random ID suitable for request
// correlation. crypto/rand.Read always returns len(b) and nil error on
// all supported platforms (see Go docs), so no error handling is needed.
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
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
// USER
// -------------------------------------------------------------------------

// WithUser stores the identity a request authenticated as, so every audit
// entry the request produces names it without any of them being handed a user.
//
// One S3 request writes two correlated entries: the HTTP-layer one the
// transport emits, and a storage-layer one emitted several packages deeper,
// where no identity is in scope and passing one would mean a signature change
// on the whole write path. The request id already crosses that distance on the
// context, and the identity travels the same way for the same reason.
func WithUser(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userKey, id)
}

// User extracts the authenticated identity from the context. Returns empty
// string for a request that has not authenticated yet, or one that failed to,
// which is what keeps a rejected request from claiming an identity it never
// proved.
func User(ctx context.Context) string {
	if id, ok := ctx.Value(userKey).(string); ok {
		return id
	}
	return ""
}

// -------------------------------------------------------------------------
// AUDIT LOGGING
// -------------------------------------------------------------------------

// Log emits a structured audit log entry at Info level. Automatically
// includes the request ID and the authenticated identity from context. If
// OnEvent is set, it is called with the event name for metrics integration.
func Log(ctx context.Context, event string, attrs ...slog.Attr) {
	if fn := onEvent.Load(); fn != nil {
		(*fn)(event)
	}

	base := []slog.Attr{
		slog.Bool("audit", true),
		slog.String("event", event),
	}

	if id := User(ctx); id != "" {
		base = append(base, slog.String("user", id))
	}

	if id := RequestID(ctx); id != "" {
		base = append(base, slog.String("request_id", id))
	}

	base = append(base, attrs...)

	slog.LogAttrs(ctx, slog.LevelInfo, "audit", base...)
}
