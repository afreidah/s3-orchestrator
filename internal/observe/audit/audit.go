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

// requestIDKey, userKey and workloadKey are the context keys the correlation
// ID, the authenticated identity and the workload are stored under.
const (
	requestIDKey contextKey = iota
	userKey
	workloadKey
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

// WithUser stores the authenticated identity on the context, so the
// storage-layer audit entry deep in the write path can name it like the
// request id.
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
// WORKLOAD
// -------------------------------------------------------------------------

// Workload names what a backend call was made for, so usage can be split by
// cause.
type Workload string

// The workloads. A charge with none is recorded as unknown.
const (
	WorkloadUnknown          Workload = "unknown"
	WorkloadClient           Workload = "client"
	WorkloadAdmin            Workload = "admin"
	WorkloadScrub            Workload = "scrub"
	WorkloadBackfill         Workload = "backfill"
	WorkloadReplication      Workload = "replication"
	WorkloadOverReplication  Workload = "over_replication"
	WorkloadRebalance        Workload = "rebalance"
	WorkloadDrain            Workload = "drain"
	WorkloadRewrite          Workload = "rewrite"
	WorkloadReconcile        Workload = "reconcile"
	WorkloadLifecycle        Workload = "lifecycle"
	WorkloadMultipartCleanup Workload = "multipart_cleanup"
	WorkloadPendingReaper    Workload = "pending_reaper"
	WorkloadCleanup          Workload = "cleanup"
	WorkloadPurge            Workload = "purge"
	WorkloadProbe            Workload = "probe"
)

// WithWorkload stores the workload on the context, replacing any outer one, so
// a scrub started by an admin request is charged as a scrub.
func WithWorkload(ctx context.Context, w Workload) context.Context {
	return context.WithValue(ctx, workloadKey, w)
}

// WorkloadOf extracts the workload from the context. Returns empty string when
// none is set.
func WorkloadOf(ctx context.Context) Workload {
	w, _ := ctx.Value(workloadKey).(Workload)
	return w
}

// -------------------------------------------------------------------------
// AUDIT LOGGING
// -------------------------------------------------------------------------

// Log emits a structured audit log entry at Info level. Automatically
// includes the request ID, the authenticated identity and the workload from
// context. If
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

	if w := WorkloadOf(ctx); w != "" {
		base = append(base, slog.String("workload", string(w)))
	}

	base = append(base, attrs...)

	slog.LogAttrs(ctx, slog.LevelInfo, "audit", base...)
}
