// -------------------------------------------------------------------------------
// Admin API Handler - Type Definition and Skeleton
//
// Author: Alex Freidah
//
// Handler/Deps types and constructor for the admin API exposed under
// /admin/api/. The per-domain endpoint implementations live in
// handler_<domain>.go siblings: route registration + auth in
// handler_routes.go; observability endpoints in handler_status.go; cache
// admin in handler_cache.go; replication + over-replication in
// handler_replication.go; backend drain/remove in handler_backends.go;
// key rotation + bulk encrypt/decrypt in handler_encryption.go;
// integrity scrub/backfill/reconcile in handler_integrity.go.
// -------------------------------------------------------------------------------

// Package admin provides the admin API handler for operational control endpoints.
package admin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// Handler serves the admin API endpoints.
type Handler struct {
	log          *slog.Logger
	backendOps   BackendOps
	dashboardOps DashboardReader
	objects      *ops.Objects
	integrity    *ops.Integrity
	replication  *ops.Replication
	rebalance    *ops.Rebalance
	encryption   *ops.Encryption
	compression  *ops.Compression
	drain        *drain.Manager
	lifecycle    core.BackendLifecycleStore
	reconciler   Reconciler
	dbHealthy    func() bool
	workerHealth func() []adminapi.WorkerHealth // nil when lifecycle manager is not wired
	logs         logReader                      // nil when the log buffer is not wired
	replMetrics  replicationSnapshotter         // nil when the metrics collector is not wired
	cleanup      core.CleanupStore
	objectCache  cache.ObjectCache
	flightRec    io.WriterTo // nil when debug.flight_recorder.enabled is false
	token        string
	logLevel     *slog.LevelVar
	// reloadStatus is the per-process snapshot of the last reload
	// result, already converted to the wire type. Set post-construction
	// by the runtime so the admin handler does not import the reload
	// package (which would cycle via UI). Returns nil before any reload
	// has happened.
	reloadStatus func() *adminapi.ReloadStatusResponse
}

// Deps groups the narrow role interfaces and infrastructure the admin
// handler touches. Each field carries the smallest contract the handler
// actually uses, so the constructor (and the backing DI provider) never
// hand the handler a god-shaped orchestration object.
type Deps struct {
	BackendOps   BackendOps
	Dashboard    DashboardReader
	Objects      *ops.Objects
	Integrity    *ops.Integrity
	Replication  *ops.Replication
	Rebalance    *ops.Rebalance
	Encryption   *ops.Encryption
	Compression  *ops.Compression
	Drain        *drain.Manager
	Lifecycle    core.BackendLifecycleStore
	DBHealthy    func() bool                    // typically *breaker.CircuitBreaker.IsHealthy
	WorkerHealth func() []adminapi.WorkerHealth // typically lifecycle.Manager.Health adapted
	LogBuffer    logReader                      // nil when the log buffer is not wired
	ReplMetrics  replicationSnapshotter         // nil when the metrics collector is not wired
	Cleanup      core.CleanupStore
	ObjectCache  cache.ObjectCache // nil when object data caching is disabled
	FlightRec    io.WriterTo       // nil when debug.flight_recorder.enabled is false
	Reconciler   Reconciler
	Token        string
	LogLevel     *slog.LevelVar
}

// New creates a new admin API handler from its narrow dependency bag.
func New(d *Deps) *Handler {
	must.NotNil("d", d)
	must.NotNil("d.BackendOps", d.BackendOps)
	must.NotNil("d.Objects", d.Objects)
	must.NotNil("d.Integrity", d.Integrity)
	must.NotNil("d.Replication", d.Replication)
	must.NotNil("d.Rebalance", d.Rebalance)
	must.NotNil("d.Encryption", d.Encryption)
	must.NotNil("d.Compression", d.Compression)
	must.NotNil("d.Drain", d.Drain)
	must.NotNil("d.Lifecycle", d.Lifecycle)
	must.NotNil("d.Cleanup", d.Cleanup)
	must.NotNil("d.LogLevel", d.LogLevel)
	return &Handler{
		log:          slog.Default().With(logfmt.Component("admin")),
		backendOps:   d.BackendOps,
		dashboardOps: d.Dashboard,
		objects:      d.Objects,
		integrity:    d.Integrity,
		replication:  d.Replication,
		rebalance:    d.Rebalance,
		encryption:   d.Encryption,
		compression:  d.Compression,
		drain:        d.Drain,
		lifecycle:    d.Lifecycle,
		reconciler:   d.Reconciler,
		dbHealthy:    d.DBHealthy,
		workerHealth: d.WorkerHealth,
		logs:         d.LogBuffer,
		replMetrics:  d.ReplMetrics,
		cleanup:      d.Cleanup,
		objectCache:  d.ObjectCache,
		flightRec:    d.FlightRec,
		token:        d.Token,
		logLevel:     d.LogLevel,
	}
}

// SetReloadStatusProvider wires the callback that returns the most recent
// reload result. Called by the runtime after the reload coordinator is
// built. Routing through a setter rather than constructor injection
// avoids the import cycle that would result from admin importing the
// reload package directly.
func (h *Handler) SetReloadStatusProvider(fn func() *adminapi.ReloadStatusResponse) {
	h.reloadStatus = fn
}

// Outcome values every operation response carries, so a client can branch on
// what happened before reading the operation-specific counts.
const (
	statusOK       = "ok"
	statusSkipped  = "skipped"
	statusComplete = "complete"
)

// skipReason reports the reason an operation declined to run, and whether it
// declined at all. An operation that skips is answering the caller, not
// failing, so every handler renders it as a successful response carrying the
// reason rather than as a server error.
func skipReason(err error) (string, bool) {
	if skip, ok := errors.AsType[*ops.SkipError](err); ok {
		return skip.Reason, true
	}
	return "", false
}

// internalError logs the underlying error against the operator-facing
// message and writes a 500 JSON response. Use for unexpected server-side
// failures where the original err is recorded but never returned to the
// caller. Extra attrs are appended to the log call so the caller can
// attach correlating fields (object key, backend name, etc.).
func (h *Handler) internalError(ctx context.Context, w http.ResponseWriter, msg string, err error, attrs ...any) {
	args := append([]any{"error", err}, attrs...)
	h.log.ErrorContext(ctx, msg, args...)
	httputil.WriteJSONError(w, http.StatusInternalServerError, msg)
}
