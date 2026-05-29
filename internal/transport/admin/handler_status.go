// -------------------------------------------------------------------------------
// Admin API - Read-Only Observability Endpoints
//
// Author: Alex Freidah
//
// Endpoints that surface state without changing it (status, reload-status,
// object-locations, cleanup-queue, workers) plus the small admin
// "control knob" endpoints (usage-flush, log-level) that fit naturally
// alongside them. Worker health is here because it is the snapshot read
// path for the lifecycle supervisor.
// -------------------------------------------------------------------------------

package admin

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// handleReloadStatus returns the most recent reload result captured by
// the reload coordinator. Returns a "no_reload_yet" placeholder when
// SIGHUP has not fired since startup or when the runtime has not
// wired the provider.
func (h *Handler) handleReloadStatus(w http.ResponseWriter, _ *http.Request) {
	if h.reloadStatus == nil {
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "no_reload_yet"})
		return
	}
	result := h.reloadStatus()
	if result == nil {
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "no_reload_yet"})
		return
	}
	httputil.WriteJSON(w, http.StatusOK, result)
}

// handleStatus returns backend health and circuit breaker state.
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	data, err := h.backendOps.GetDashboardData(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch status", err)
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

	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"db_healthy":   h.dbHealthy(),
		"backends":     backends,
		"usage_period": data.UsagePeriod,
	})
}

// handleObjectLocations returns all copies of an object across backends.
func (h *Handler) handleObjectLocations(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "key parameter is required")
		return
	}

	locations, err := h.objects.GetAllObjectLocations(r.Context(), key)
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch locations", err, slog.String("key", key))
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]any{"key": key, "locations": locations})
}

// handleCleanupQueue returns cleanup queue depth and pending items.
func (h *Handler) handleCleanupQueue(w http.ResponseWriter, r *http.Request) {
	depth, err := h.cleanup.CleanupQueueDepth(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch cleanup queue", err)
		return
	}

	items, err := h.cleanup.GetPendingCleanups(r.Context(), 50)
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch cleanup queue", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]any{"depth": depth, "items": items})
}

// handleUsageFlush forces a flush of usage counters to the database.
func (h *Handler) handleUsageFlush(w http.ResponseWriter, r *http.Request) {
	if err := h.backendOps.FlushUsage(r.Context()); err != nil {
		h.internalError(r.Context(), w, "flush failed", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "flushed"})
}

// handleLogLevel gets or sets the runtime log level.
func (h *Handler) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"level": strings.ToLower(h.logLevel.Level().String())})
		return
	}

	var req struct {
		Level string `json:"level"`
	}
	if !httputil.DecodeJSONBody(w, r, &req, 1<<20) {
		return
	}
	parsed := config.ParseLogLevel(req.Level)
	h.logLevel.Set(parsed)
	h.log.InfoContext(r.Context(), "log level changed via admin API", "level", req.Level)
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"level": strings.ToLower(parsed.String())})
}

// WorkerHealth is the JSON shape returned by /admin/api/workers. Mirrors
// lifecycle.WorkerHealth but lives here so the admin transport package
// owns its own response contract and does not import the lifecycle
// package directly. Field tags must stay in lockstep with the source
// type or the wire format silently diverges.
type WorkerHealth struct {
	Name                string    `json:"name"`
	LastSuccess         time.Time `json:"last_success"`
	LastFailure         time.Time `json:"last_failure"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

// handleWorkers returns a snapshot of every registered background
// service's last-tick health. The supervisor records a tick outcome
// after every fire, so operators can identify stalled or repeatedly
// failing workers without scraping logs. Returns 503 when the
// lifecycle manager was not wired (proxy-only deployments that disable
// the worker pool).
func (h *Handler) handleWorkers(w http.ResponseWriter, _ *http.Request) {
	if h.workerHealth == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "worker health not available")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"workers": h.workerHealth(),
	})
}
