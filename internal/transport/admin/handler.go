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
	"log/slog"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// BackendOps is the narrow surface of *proxy.BackendManager that the admin
// handler depends on for operations not encapsulated by a named sub-manager
// (replicator, drain, scrubber, etc.). *proxy.BackendManager satisfies it.
type BackendOps interface {
	GetDashboardData(ctx context.Context) (*dashboard.Data, error)
	FlushUsage(ctx context.Context) error
	UpdateQuotaMetrics(ctx context.Context) error
	RecordUsage(backendName string, requests, ingressBytes, egressBytes int64)
	GetBackend(name string) (backend.ObjectBackend, error)
	IntegrityConfig() *config.IntegrityConfig
}

// Compile-time assertion: *proxy.BackendManager implements BackendOps.
var _ BackendOps = (*proxy.BackendManager)(nil)

// Handler serves the admin API endpoints.
type Handler struct {
	log          *slog.Logger
	backendOps   BackendOps
	replicator   ReplicatorOps
	overRep      OverReplicationOps
	drain        *drain.Manager
	scrubber     ScrubberOps
	lifecycle    core.BackendLifecycleStore
	reconciler   Reconciler
	dbHealthy    func() bool
	workerHealth func() []WorkerHealth // nil when lifecycle manager is not wired
	objects      core.ObjectStore
	cleanup      core.CleanupStore
	encAdmin     core.EncryptionAdmin
	encryptor    *encryption.Encryptor
	objectCache  cache.ObjectCache
	token        string
	logLevel     *slog.LevelVar
	// reloadStatus is the per-process snapshot of the last reload
	// result. Set post-construction by the runtime so the admin handler
	// does not import the reload package (which would cycle via UI).
	// Returns nil before any reload has happened.
	reloadStatus func() any
}

// Deps groups the narrow role interfaces and infrastructure the admin
// handler touches. Each field carries the smallest contract the handler
// actually uses, so the constructor (and the backing DI provider) never
// hand the handler a god-shaped *proxy.BackendManager.
type Deps struct {
	BackendOps   BackendOps
	Replicator   ReplicatorOps
	OverRep      OverReplicationOps
	Drain        *drain.Manager
	Scrubber     ScrubberOps
	Lifecycle    core.BackendLifecycleStore
	DBHealthy    func() bool            // typically *breaker.CircuitBreaker.IsHealthy
	WorkerHealth func() []WorkerHealth // typically lifecycle.Manager.Health adapted
	Encryption   core.EncryptionAdmin
	Objects      core.ObjectStore
	Cleanup      core.CleanupStore
	Encryptor    *encryption.Encryptor
	ObjectCache  cache.ObjectCache // nil when object data caching is disabled
	Reconciler   Reconciler
	Token        string
	LogLevel     *slog.LevelVar
}

// New creates a new admin API handler from its narrow dependency bag.
func New(d *Deps) *Handler {
	return &Handler{
		log:          slog.Default().With(logfmt.Component("admin")),
		backendOps:   d.BackendOps,
		replicator:   d.Replicator,
		overRep:      d.OverRep,
		drain:        d.Drain,
		scrubber:     d.Scrubber,
		lifecycle:    d.Lifecycle,
		reconciler:   d.Reconciler,
		dbHealthy:    d.DBHealthy,
		workerHealth: d.WorkerHealth,
		objects:      d.Objects,
		cleanup:      d.Cleanup,
		encAdmin:     d.Encryption,
		encryptor:    d.Encryptor,
		objectCache:  d.ObjectCache,
		token:        d.Token,
		logLevel:     d.LogLevel,
	}
}

// SetReloadStatusProvider wires the callback that returns the most recent
// reload result. Called by the runtime after the reload coordinator is
// built. Routing through a setter rather than constructor injection
// avoids the import cycle that would result from admin importing the
// reload package directly.
func (h *Handler) SetReloadStatusProvider(fn func() any) {
	h.reloadStatus = fn
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
