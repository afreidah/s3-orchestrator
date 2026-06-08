// -------------------------------------------------------------------------------
// Admin API - Route Registration and Auth Middleware
//
// Author: Alex Freidah
//
// Every admin route lands here so the route table stays grep-able from one
// file. requireToken is the shared authentication middleware applied to
// every route; per-endpoint authorization (if any) lives inside the
// handler itself.
// -------------------------------------------------------------------------------

package admin

import (
	"crypto/subtle"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// Register mounts the admin API routes on the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/status", h.requireToken(h.handleStatus))
	mux.HandleFunc("GET /admin/api/reload-status", h.requireToken(h.handleReloadStatus))
	mux.HandleFunc("GET /admin/api/object-locations", h.requireToken(h.handleObjectLocations))
	mux.HandleFunc("GET /admin/api/cleanup-queue", h.requireToken(h.handleCleanupQueue))
	mux.HandleFunc("POST /admin/api/usage-flush", h.requireToken(h.handleUsageFlush))
	mux.HandleFunc("POST /admin/api/usage-reconcile", h.requireToken(h.handleReconcileUsage))
	mux.HandleFunc("POST /admin/api/replicate", h.requireToken(h.handleReplicate))
	mux.HandleFunc("GET /admin/api/log-level", h.requireToken(h.handleLogLevel))
	mux.HandleFunc("PUT /admin/api/log-level", h.requireToken(h.handleLogLevel))
	mux.HandleFunc("POST /admin/api/backends/{name}/drain", h.requireToken(h.handleStartDrain))
	mux.HandleFunc("GET /admin/api/backends/{name}/drain", h.requireToken(h.handleDrainProgress))
	mux.HandleFunc("DELETE /admin/api/backends/{name}/drain", h.requireToken(h.handleCancelDrain))
	mux.HandleFunc("DELETE /admin/api/backends/{name}", h.requireToken(h.handleRemoveBackend))
	mux.HandleFunc("GET /admin/api/over-replication", h.requireToken(h.handleOverReplicationStatus))
	mux.HandleFunc("POST /admin/api/over-replication", h.requireToken(h.handleOverReplicationClean))
	mux.HandleFunc("POST /admin/api/rotate-encryption-key", h.requireToken(h.handleRotateEncryptionKey))
	mux.HandleFunc("POST /admin/api/encrypt-existing", h.requireToken(h.handleEncryptExisting))
	mux.HandleFunc("POST /admin/api/decrypt-existing", h.requireToken(h.handleDecryptExisting))
	mux.HandleFunc("POST /admin/api/scrub", h.requireToken(h.handleScrub))
	mux.HandleFunc("POST /admin/api/backfill-checksums", h.requireToken(h.handleBackfillChecksums))
	mux.HandleFunc("POST /admin/api/reconcile", h.requireToken(h.handleReconcile))
	mux.HandleFunc("POST /admin/api/cache/flush", h.requireToken(h.handleCacheFlush))
	mux.HandleFunc("GET /admin/api/cache", h.requireToken(h.handleCacheStats))
	mux.HandleFunc("DELETE /admin/api/cache/keys/{key...}", h.requireToken(h.handleCacheInvalidateKey))
	mux.HandleFunc("DELETE /admin/api/cache/prefix", h.requireToken(h.handleCacheInvalidatePrefix))
	mux.HandleFunc("GET /admin/api/workers", h.requireToken(h.handleWorkers))
	mux.HandleFunc("POST /admin/api/trace/snapshot", h.requireToken(h.handleTraceSnapshot))
}

// requireToken wraps a handler and enforces X-Admin-Token authentication.
func (h *Handler) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) != 1 {
			h.log.WarnContext(r.Context(), "unauthorized request", "path", r.URL.Path, "client_addr", r.RemoteAddr)
			httputil.WriteJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}
