// -------------------------------------------------------------------------------
// Admin API Handler - Operational Control Endpoints
//
// Author: Alex Freidah
//
// HTTP handler for administrative operations exposed under /admin/api/. Provides
// endpoints for checking system status, inspecting object locations, managing the
// cleanup queue, flushing usage counters, triggering replication, and controlling
// the runtime log level. All endpoints require a valid X-Admin-Token header.
// -------------------------------------------------------------------------------

// Package admin provides the admin API handler for operational control endpoints.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/storage"
)

// Handler serves the admin API endpoints.
type Handler struct {
	manager  *storage.BackendManager
	store    *storage.CircuitBreakerStore
	token    string
	logLevel *slog.LevelVar
}

// New creates a new admin API handler.
func New(manager *storage.BackendManager, store *storage.CircuitBreakerStore, token string, logLevel *slog.LevelVar) *Handler {
	return &Handler{
		manager:  manager,
		store:    store,
		token:    token,
		logLevel: logLevel,
	}
}

// Register mounts the admin API routes on the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/admin/api/status", h.requireToken(h.handleStatus))
	mux.HandleFunc("/admin/api/object-locations", h.requireToken(h.handleObjectLocations))
	mux.HandleFunc("/admin/api/cleanup-queue", h.requireToken(h.handleCleanupQueue))
	mux.HandleFunc("/admin/api/usage-flush", h.requireToken(h.handleUsageFlush))
	mux.HandleFunc("/admin/api/replicate", h.requireToken(h.handleReplicate))
	mux.HandleFunc("/admin/api/log-level", h.requireToken(h.handleLogLevel))
	mux.HandleFunc("POST /admin/api/backends/{name}/drain", h.requireToken(h.handleStartDrain))
	mux.HandleFunc("GET /admin/api/backends/{name}/drain", h.requireToken(h.handleDrainProgress))
	mux.HandleFunc("DELETE /admin/api/backends/{name}/drain", h.requireToken(h.handleCancelDrain))
	mux.HandleFunc("DELETE /admin/api/backends/{name}", h.requireToken(h.handleRemoveBackend))
}

// -------------------------------------------------------------------------
// AUTH MIDDLEWARE
// -------------------------------------------------------------------------

// requireToken wraps a handler and enforces X-Admin-Token authentication.
func (h *Handler) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Admin-Token")
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// -------------------------------------------------------------------------
// HANDLERS
// -------------------------------------------------------------------------

// handleStatus returns backend health and circuit breaker state.
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	data, err := h.manager.GetDashboardData(r.Context())
	if err != nil {
		slog.Error("Admin: failed to fetch status", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch status"})
		return
	}

	type backendStatus struct {
		Name         string `json:"name"`
		BytesUsed    int64  `json:"bytes_used"`
		BytesLimit   int64  `json:"bytes_limit"`
		ObjectCount  int64  `json:"object_count"`
		APIRequests  int64  `json:"api_requests"`
		EgressBytes  int64  `json:"egress_bytes"`
		IngressBytes int64  `json:"ingress_bytes"`
	}

	backends := make([]backendStatus, 0, len(data.BackendOrder))
	for _, name := range data.BackendOrder {
		bs := backendStatus{Name: name}
		if qs, ok := data.QuotaStats[name]; ok {
			bs.BytesUsed = qs.BytesUsed
			bs.BytesLimit = qs.BytesLimit
		}
		if oc, ok := data.ObjectCounts[name]; ok {
			bs.ObjectCount = oc
		}
		if us, ok := data.UsageStats[name]; ok {
			bs.APIRequests = us.APIRequests
			bs.EgressBytes = us.EgressBytes
			bs.IngressBytes = us.IngressBytes
		}
		backends = append(backends, bs)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"db_healthy":   h.store.IsHealthy(),
		"backends":     backends,
		"usage_period": data.UsagePeriod,
	})
}

// handleObjectLocations returns all copies of an object across backends.
func (h *Handler) handleObjectLocations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key parameter is required"})
		return
	}

	locations, err := h.store.GetAllObjectLocations(r.Context(), key)
	if err != nil {
		slog.Error("Admin: failed to fetch object locations", "key", key, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch locations"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"key": key, "locations": locations})
}

// handleCleanupQueue returns cleanup queue depth and pending items.
func (h *Handler) handleCleanupQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	depth, err := h.store.CleanupQueueDepth(r.Context())
	if err != nil {
		slog.Error("Admin: failed to fetch cleanup queue depth", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch cleanup queue"})
		return
	}

	items, err := h.store.GetPendingCleanups(r.Context(), 50)
	if err != nil {
		slog.Error("Admin: failed to fetch pending cleanups", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch cleanup queue"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"depth": depth, "items": items})
}

// handleUsageFlush forces a flush of usage counters to the database.
func (h *Handler) handleUsageFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	if err := h.manager.FlushUsage(r.Context()); err != nil {
		slog.Error("Admin: usage flush failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "flush failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "flushed"})
}

// handleReplicate triggers one replication cycle.
func (h *Handler) handleReplicate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	rcfg := h.manager.ReplicationConfig()
	if rcfg == nil || rcfg.Factor <= 1 {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "skipped",
			"copies_created": 0,
			"reason":         "replication not configured or factor <= 1",
		})
		return
	}

	created, err := h.manager.Replicate(r.Context(), *rcfg)
	if err != nil {
		slog.Error("Admin: replication failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "replication failed"})
		return
	}

	if err := h.manager.UpdateQuotaMetrics(r.Context()); err != nil {
		slog.Warn("Failed to update quota metrics after admin replicate", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "copies_created": created})
}

// handleLogLevel gets or sets the runtime log level.
func (h *Handler) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"level": strings.ToLower(h.logLevel.Level().String())})

	case http.MethodPut:
		var req struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		parsed := config.ParseLogLevel(req.Level)
		h.logLevel.Set(parsed)
		slog.Info("Log level changed via admin API", "level", req.Level)
		writeJSON(w, http.StatusOK, map[string]string{"level": strings.ToLower(parsed.String())})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// -------------------------------------------------------------------------
// BACKEND LIFECYCLE HANDLERS
// -------------------------------------------------------------------------

// handleStartDrain begins draining a backend.
func (h *Handler) handleStartDrain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.manager.StartDrain(r.Context(), name); err != nil {
		slog.Error("Admin: drain start failed", "backend", name, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "drain operation failed"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "drain started", "backend": name})
}

// handleDrainProgress returns the current state of a drain operation.
func (h *Handler) handleDrainProgress(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	progress, err := h.manager.GetDrainProgress(r.Context(), name)
	if err != nil {
		slog.Error("Admin: drain progress failed", "backend", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "drain operation failed"})
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

// handleCancelDrain cancels an active drain operation.
func (h *Handler) handleCancelDrain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.manager.CancelDrain(name); err != nil {
		slog.Error("Admin: drain cancel failed", "backend", name, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "drain operation failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "drain cancelled", "backend": name})
}

// handleRemoveBackend deletes all DB records for a backend.
func (h *Handler) handleRemoveBackend(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	purge := r.URL.Query().Get("purge") == "true"

	if err := h.manager.RemoveBackend(r.Context(), name, purge); err != nil {
		slog.Error("Admin: remove backend failed", "backend", name, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "remove failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "backend removed", "backend": name})
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

